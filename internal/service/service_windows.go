//go:build windows

package service

import (
	"context"
	"fmt"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"unsafe"
)

// Windows service control constants (see winsvc.h).
const (
	serviceWin32OwnProcess  = 0x00000010
	serviceAcceptStop       = 0x00000001
	serviceAcceptShutdown   = 0x00000004
	serviceRunning          = 0x00000004
	serviceStartPending     = 0x00000002
	serviceStopPending      = 0x00000003
	serviceStopped          = 0x00000001
	serviceControlStop      = 0x00000001
	serviceControlShutdown  = 0x00000005
	noError                 = 0
	errServiceNotConnected  = syscall.Errno(1063) // ERROR_FAILED_SERVICE_CONTROLLER_CONNECT
	serviceStopWaitHintMs   = 10000
	serviceStartWaitHintMs  = 5000
	scmBinPathArgumentQuirk = "binPath="
)

type serviceStatus struct {
	ServiceType             uint32
	CurrentState            uint32
	ControlsAccepted        uint32
	Win32ExitCode           uint32
	ServiceSpecificExitCode uint32
	CheckPoint              uint32
	WaitHint                uint32
}

type serviceTableEntry struct {
	ServiceName *uint16
	ServiceProc uintptr
}

var (
	advapi32                          = syscall.NewLazyDLL("advapi32.dll")
	procStartServiceCtrlDispatcherW   = advapi32.NewProc("StartServiceCtrlDispatcherW")
	procRegisterServiceCtrlHandlerExW = advapi32.NewProc("RegisterServiceCtrlHandlerExW")
	procSetServiceStatus              = advapi32.NewProc("SetServiceStatus")
)

var (
	svcMu    sync.Mutex
	svcState *svcRun
)

type svcRun struct {
	name    string
	namePtr *uint16
	handle  uintptr
	status  serviceStatus
	run     func(context.Context) error
	cancel  context.CancelFunc
	done    chan struct{}
	err     error
}

// RunInForeground connects the current process to the Windows service
// controller and runs the job as a service.
//
// It returns (false, nil) when the process was not started by the service
// controller (for example `tidesync service run` in a terminal); the caller
// then simply runs the job in the foreground.
func RunInForeground(o Options, run func(context.Context) error) (bool, error) {
	name := o.NameOrDefault()
	namePtr, err := syscall.UTF16PtrFromString(name)
	if err != nil {
		return false, err
	}
	st := &svcRun{name: name, namePtr: namePtr, run: run, done: make(chan struct{})}
	svcMu.Lock()
	svcState = st
	svcMu.Unlock()

	cb := syscall.NewCallback(serviceMainCallback)
	table := [2]serviceTableEntry{{ServiceName: namePtr, ServiceProc: cb}}

	// StartServiceCtrlDispatcher must be called from the thread that will host
	// the service; it blocks until every service has stopped.
	runtime.LockOSThread()
	ret, _, callErr := procStartServiceCtrlDispatcherW.Call(uintptr(unsafe.Pointer(&table[0])))
	runtime.UnlockOSThread()

	if ret == 0 {
		if errno, ok := callErr.(syscall.Errno); ok && errno == errServiceNotConnected {
			return false, nil
		}
		return false, fmt.Errorf("cannot connect to the Windows service controller: %v", callErr)
	}
	return true, st.err
}

func serviceMainCallback(argc uint32, argv **uint16) uintptr {
	svcMu.Lock()
	st := svcState
	svcMu.Unlock()
	if st == nil {
		return 0
	}
	st.serviceMain()
	return 0
}

func serviceHandlerCallback(ctrl, eventType uint32, eventData, ctx uintptr) uintptr {
	svcMu.Lock()
	st := svcState
	svcMu.Unlock()
	if st == nil {
		return noError
	}
	switch ctrl {
	case serviceControlStop, serviceControlShutdown:
		st.setStatus(serviceStopPending, 0, serviceStopWaitHintMs)
		if st.cancel != nil {
			st.cancel()
		}
	}
	return noError
}

func (s *svcRun) serviceMain() {
	handlerCb := syscall.NewCallback(serviceHandlerCallback)
	h, _, regErr := procRegisterServiceCtrlHandlerExW.Call(
		uintptr(unsafe.Pointer(s.namePtr)), handlerCb, 0)
	if h == 0 {
		s.err = fmt.Errorf("RegisterServiceCtrlHandlerEx failed: %v", regErr)
		return
	}
	s.handle = h
	s.setStatus(serviceStartPending, 0, serviceStartWaitHintMs)

	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	go func() {
		defer close(s.done)
		s.err = s.run(ctx)
	}()

	s.setStatus(serviceRunning, serviceAcceptStop|serviceAcceptShutdown, 0)
	<-s.done
	s.setStatus(serviceStopped, 0, 0)
}

func (s *svcRun) setStatus(state, accepted, waitHint uint32) {
	if s.handle == 0 {
		return
	}
	s.status.ServiceType = serviceWin32OwnProcess
	s.status.CurrentState = state
	s.status.ControlsAccepted = accepted
	s.status.Win32ExitCode = 0
	s.status.WaitHint = waitHint
	procSetServiceStatus.Call(s.handle, uintptr(unsafe.Pointer(&s.status)))
}

// ---- installation ---------------------------------------------------------

// Install registers either a scheduled task (default, robust and needs no
// service host) or a real Windows service (mode "scm").
func Install(o Options) (string, error) {
	if err := o.Validate(); err != nil {
		return "", err
	}
	if o.Mode == "scm" {
		return installSCM(o)
	}
	return installTask(o)
}

// installTask registers a Task Scheduler job that runs `sync -once` on the
// configured interval. This is the recommended Windows deployment: it survives
// reboots, needs no service host, and tolerates long running synchronisations.
func installTask(o Options) (string, error) {
	name := o.NameOrDefault()
	if _, ok := lookPath("schtasks"); !ok {
		return "", fmt.Errorf("schtasks.exe not found; create the task manually: %s", taskManualHint(o))
	}
	minutes := int(o.IntervalOrDefault().Minutes())
	if minutes < 1 {
		minutes = 1
	}
	if minutes > 1439 {
		minutes = 1439
	}
	action := QuoteCommandLine(append([]string{o.Binary}, o.OneShotArgs()...))
	args := []string{"/Create", "/F", "/TN", name, "/SC", "MINUTE", "/MO", strconv.Itoa(minutes), "/TR", action}
	// SYSTEM runs the task without an interactive logon, which needs elevation.
	args = append(args, "/RU", "SYSTEM", "/RL", "HIGHEST")
	out, err := runCommand("schtasks", args...)
	if err != nil {
		return "", fmt.Errorf(`%v

Install the task from an elevated prompt (Run as Administrator), or create it
by hand:

  %s

Or install a real Windows service instead:

  tidesync service install -config %s -mode scm`, err, taskManualHint(o), o.Config)
	}
	return fmt.Sprintf("scheduled task %q created (every %d minute(s))\n%s", name, minutes, out), nil
}

func taskManualHint(o Options) string {
	minutes := int(o.IntervalOrDefault().Minutes())
	if minutes < 1 {
		minutes = 1
	}
	return fmt.Sprintf("schtasks /Create /F /TN %s /SC MINUTE /MO %d /RU SYSTEM /RL HIGHEST /TR \"%s\"",
		o.NameOrDefault(), minutes, QuoteCommandLine(append([]string{o.Binary}, o.OneShotArgs()...)))
}

// installSCM registers a classic Windows service that runs the daemon.
func installSCM(o Options) (string, error) {
	name := o.NameOrDefault()
	if _, ok := lookPath("sc.exe"); !ok {
		if _, ok := lookPath("sc"); !ok {
			return "", fmt.Errorf("sc.exe not found")
		}
	}
	scExe := "sc.exe"
	if _, ok := lookPath(scExe); !ok {
		scExe = "sc"
	}
	// Install the service first, then remove any previous registration.
	_, _ = runCommand(scExe, "stop", name)
	_, _ = runCommand(scExe, "delete", name)

	binPath := QuoteCommandLine(append([]string{o.Binary}, "service", "run", "-config", o.Config))
	out, err := runCommand(scExe, "create", name, scmBinPathArgumentQuirk, binPath,
		"start=", "auto", "DisplayName=", o.DisplayName())
	if err != nil {
		return "", fmt.Errorf("%v\n\nCreating a service needs an elevated prompt (Run as Administrator). Alternative: tidesync service install -mode task", err)
	}
	_, _ = runCommand(scExe, "description", name, o.DisplayName()+" ("+HostDescription()+")")
	_, _ = runCommand(scExe, "failure", name, "reset=", "86400", "actions=", "restart/60000/restart/60000/restart/60000")
	if _, err := runCommand(scExe, "start", name); err != nil {
		return out, fmt.Errorf("service created but did not start: %v", err)
	}
	return fmt.Sprintf("Windows service %q installed and started", name), nil
}

// Uninstall removes the task and/or the service.
func Uninstall(o Options) (string, error) {
	name := o.NameOrDefault()
	removed := []string{}
	if _, ok := lookPath("schtasks"); ok {
		if _, err := runCommand("schtasks", "/Delete", "/F", "/TN", name); err == nil {
			removed = append(removed, "scheduled task "+name)
		}
	}
	scExe := "sc.exe"
	if _, ok := lookPath(scExe); !ok {
		scExe = "sc"
	}
	_, _ = runCommand(scExe, "stop", name)
	if _, err := runCommand(scExe, "delete", name); err == nil {
		removed = append(removed, "service "+name)
	}
	if len(removed) == 0 {
		return "", fmt.Errorf("nothing named %q was installed (or the operation needs an elevated prompt)", name)
	}
	return "removed " + strings.Join(removed, " and "), nil
}

// Status reports the state of the task or service.
func Status(o Options) (StatusInfo, error) {
	name := o.NameOrDefault()
	info := StatusInfo{}
	if _, ok := lookPath("schtasks"); ok {
		out, err := runCommand("schtasks", "/Query", "/TN", name, "/FO", "LIST", "/V")
		if err == nil {
			info.Installed = true
			info.Manager = "Task Scheduler"
			info.Unit = name
			info.Detail = out
			for _, line := range strings.Split(out, "\n") {
				l := strings.ToLower(strings.TrimSpace(line))
				if strings.HasPrefix(l, "status:") || strings.HasPrefix(l, "状态:") {
					if strings.Contains(l, "running") || strings.Contains(l, "正在运行") {
						info.Running = true
					}
				}
			}
			return info, nil
		}
	}
	scExe := "sc.exe"
	if _, ok := lookPath(scExe); !ok {
		scExe = "sc"
	}
	out, err := runCommand(scExe, "query", name)
	if err == nil {
		info.Installed = true
		info.Manager = "Windows Service"
		info.Unit = name
		info.Detail = out
		info.Running = strings.Contains(out, "RUNNING")
		return info, nil
	}
	info.Detail = "not installed"
	return info, nil
}

// Start runs the task or service now.
func Start(o Options) (string, error) {
	name := o.NameOrDefault()
	if _, ok := lookPath("schtasks"); ok {
		if out, err := runCommand("schtasks", "/Run", "/TN", name); err == nil {
			return out, nil
		}
	}
	scExe := "sc.exe"
	if _, ok := lookPath(scExe); !ok {
		scExe = "sc"
	}
	return runCommand(scExe, "start", name)
}

// Stop stops a running task or service.
func Stop(o Options) (string, error) {
	name := o.NameOrDefault()
	if _, ok := lookPath("schtasks"); ok {
		if out, err := runCommand("schtasks", "/End", "/TN", name); err == nil {
			return out, nil
		}
	}
	scExe := "sc.exe"
	if _, ok := lookPath(scExe); !ok {
		scExe = "sc"
	}
	return runCommand(scExe, "stop", name)
}
