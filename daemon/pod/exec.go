package pod

import (
	"encoding/json"
	"fmt"
	"io"
	"syscall"
	"time"

	"github.com/docker/docker/pkg/stdcopy"

	"github.com/hyperhq/hypercontainer-utils/hlog"
	"github.com/hyperhq/hyperd/utils"
	"github.com/hyperhq/runv/hypervisor"
)

type Exec struct {
	Id        string
	Container string
	Cmds      []string
	Terminal  bool
	ExitCode  uint8

	logPrefix string
	finChan   chan bool
}

func (e *Exec) LogPrefix() string {
	return e.logPrefix
}

func (e *Exec) Log(level hlog.LogLevel, args ...interface{}) {
	hlog.HLog(level, e, 1, args...)
}

func (p *XPod) CreateExec(containerId, cmds string, terminal bool) (string, error) {
	c, ok := p.containers[containerId]
	if !ok {
		err := fmt.Errorf("no container available for exec %s", cmds)
		p.Log(ERROR, err)
		return "", err
	}

	if !c.IsAlive() {
		err := fmt.Errorf("container is not available (%v) for exec %s", c.CurrentState(), cmds)
		p.Log(ERROR, err)
		return "", err
	}
	var command []string
	if err := json.Unmarshal([]byte(cmds), &command); err != nil {
		return "", err
	}

	execId := fmt.Sprintf("exec-%s", utils.RandStr(10, "alpha"))

	p.statusLock.Lock()
	p.execs[execId] = &Exec{
		Container: containerId,
		Id:        execId,
		Cmds:      command,
		Terminal:  terminal,
		ExitCode:  255,
		logPrefix: fmt.Sprintf("Pod[%s] Con[%s] Exec[%s] ", p.Id(), containerId[:12], execId),
		finChan:   make(chan bool, 1),
	}
	p.statusLock.Unlock()

	return execId, nil
}

type waitClose struct {
	io.ReadCloser
	wait chan bool
}

func (wc *waitClose) Close() error {
	close(wc.wait)
	return wc.ReadCloser.Close()
}

func (p *XPod) StartExec(stdin io.ReadCloser, stdout io.WriteCloser, containerId, execId string) error {
	c, ok := p.containers[containerId]
	if !ok {
		err := fmt.Errorf("no container %s available for exec %s", containerId, execId)
		p.Log(ERROR, err)
		return err
	}

	p.statusLock.RLock()
	es, ok := p.execs[execId]
	p.statusLock.RUnlock()

	if !ok {
		err := fmt.Errorf("no exec %s exists for container %s", execId, containerId)
		p.Log(ERROR, err)
		return err
	}

	wReader := &waitClose{ReadCloser: stdin, wait: make(chan bool)}
	tty := &hypervisor.TtyIO{
		Stdin:  wReader,
		Stdout: stdout,
	}

	if !es.Terminal && stdout != nil {
		tty.Stderr = stdcopy.NewStdWriter(stdout, stdcopy.Stderr)
		tty.Stdout = stdcopy.NewStdWriter(stdout, stdcopy.Stdout)
	}

	var fin = true
	for fin {
		select {
		case fin = <-es.finChan:
			es.Log(DEBUG, "try to drain the sync chan")
		default:
			fin = false
			es.Log(DEBUG, "the sync chan is empty")
		}
	}

	go func(es *Exec) {
		result := p.sandbox.WaitProcess(false, []string{execId}, -1)
		if result == nil {
			es.Log(ERROR, "can not wait exec")
			return
		}

		r, ok := <-result
		if !ok {
			es.Log(ERROR, "waiting exec interrupted")
			return
		}

		es.Log(DEBUG, "exec terminated at %v with code %d", r.FinishedAt, r.Code)
		es.ExitCode = uint8(r.Code)
		select {
		case es.finChan <- true:
			es.Log(DEBUG, "wake exec stopped chan")
		default:
			es.Log(WARNING, "exec already set as stopped")
		}
	}(es)

	var envs []string
	for e, v := range c.descript.Envs {
		envs = append(envs, fmt.Sprintf("%s=%s", e, v))
	}

	err := p.sandbox.AddProcess(es.Container, es.Id, es.Terminal, es.Cmds, envs, c.descript.Workdir, tty)
	<-wReader.wait
	return err
}

func (p *XPod) GetExecExitCode(containerId, execId string) (uint8, error) {
	p.statusLock.RLock()
	es, ok := p.execs[execId]
	p.statusLock.RUnlock()

	if !ok {
		err := fmt.Errorf("no exec %s exists for container %s", execId, containerId)
		p.Log(ERROR, err)
		return 255, err
	}

	select {
	case <-es.finChan:
		es.finChan <- true
	case <-time.After(time.Second * 10):
		err := fmt.Errorf("wait exec exit code timeout")
		es.Log(ERROR, err)
		return 255, err
	}
	es.Log(INFO, "got exec exit code: %d", es.ExitCode)
	return es.ExitCode, nil
}

func (p *XPod) KillExec(execId string, sig int64) error {
	p.statusLock.RLock()
	es, ok := p.execs[execId]
	p.statusLock.RUnlock()

	if !ok {
		err := fmt.Errorf("no exec %s exists for pod %s", execId, p.Id)
		p.Log(ERROR, err)
		return err
	}

	return p.protectedSandboxOperation(
		func(sb *hypervisor.Vm) error {
			return sb.SignalProcess(es.Container, es.Id, syscall.Signal(sig))
		},
		time.Second*5,
		fmt.Sprintf("Kill process %s with %d", es.Id, sig))
}

func (p *XPod) DeleteExec(containerId, execId string) {
	p.statusLock.Lock()
	delete(p.execs, execId)
	p.statusLock.Unlock()
}

func (p *XPod) CleanupExecs() {
	p.statusLock.Lock()
	p.execs = make(map[string]*Exec)
	p.statusLock.Unlock()
}

func (p *XPod) ExecVM(cmd string, stdin io.ReadCloser, stdout, stderr io.WriteCloser) (int, error) {
	return p.sandbox.HyperstartExec(cmd, &hypervisor.TtyIO{
		Stdin:  stdin,
		Stdout: stdout,
		Stderr: stderr,
	})
}
