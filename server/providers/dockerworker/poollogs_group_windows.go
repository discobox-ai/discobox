package dockerworker

import (
	"os/exec"
	"unsafe"

	"golang.org/x/sys/windows"
)

// processGroup ties a log command's descendants to it so one stop reaches them
// all. Windows has no process group that Kill reaches, so the job object is
// what plays the part: a process assigned to a job carries its children into
// it, and terminating the job terminates all of them. Without it, killing the
// shell of a `sh -c '... | tee'` would leave the tool alive holding this
// stream's write end, and the reaping wait would never finish.
type processGroup struct {
	cmd *exec.Cmd
	job windows.Handle
}

func newProcessGroup(cmd *exec.Cmd) *processGroup {
	g := &processGroup{cmd: cmd}
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		// No job to be had; kill still stops the command itself.
		return g
	}
	// Kill-on-close so a crash of this process cannot strand the tree.
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{
		BasicLimitInformation: windows.JOBOBJECT_BASIC_LIMIT_INFORMATION{
			LimitFlags: windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE,
		},
	}
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		_ = windows.CloseHandle(job)
		return g
	}
	g.job = job
	return g
}

// adopt puts the started command in the job. It cannot happen before the start
// — there is no process to assign until then — so a command that spawns a
// child in the first instants after exec can in principle beat the assignment.
// os/exec offers no suspended start to close that window, and the commands run
// here are shells that have yet to read their script by the time this returns.
func (g *processGroup) adopt() {
	if g.job == 0 || g.cmd.Process == nil {
		return
	}
	handle, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(g.cmd.Process.Pid))
	if err != nil {
		return
	}
	defer func() { _ = windows.CloseHandle(handle) }()
	if err := windows.AssignProcessToJobObject(g.job, handle); err != nil {
		// Already in a job that forbids nesting; kill still reaches the
		// command, and the goroutine reaps whatever outlives it.
		_ = windows.CloseHandle(g.job)
		g.job = 0
	}
}

func (g *processGroup) kill() {
	if g.job != 0 {
		_ = windows.TerminateJobObject(g.job, 1)
		_ = windows.CloseHandle(g.job)
		g.job = 0
		return
	}
	if g.cmd.Process != nil {
		_ = g.cmd.Process.Kill()
	}
}
