package pod

import (
	"fmt"
	"io"

	"github.com/docker/docker/pkg/stdcopy"

	"github.com/hyperhq/hyperd/utils"
	"github.com/hyperhq/runv/hypervisor"
	"github.com/hyperhq/runv/hypervisor/types"
)

type Exec struct {
	Id        string
	Container string
	Cmds      string
	Terminal  bool
	ExitCode  uint8
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

	execId := fmt.Sprintf("exec-%s", utils.RandStr(10, "alpha"))

	p.statusLock.Lock()
	p.execs[execId] = &Exec{
		Container: containerId,
		Id:        execId,
		Cmds:      cmds,
		Terminal:  terminal,
		ExitCode:  255,
	}
	p.statusLock.Unlock()

	return execId, nil
}

func (p *XPod) StartExec(stdin io.ReadCloser, stdout io.WriteCloser, containerId, execId string) error {
	p.statusLock.RLock()
	es, ok := p.execs[execId]
	p.statusLock.RUnlock()

	if !ok {
		err := fmt.Errorf("no exec %s exists for container %s", execId, containerId)
		p.Log(ERROR, err)
		return err
	}

	tty := &hypervisor.TtyIO{
		Stdin:    stdin,
		Stdout:   stdout,
		Callback: make(chan *types.VmResponse, 1),
	}

	if !es.Terminal && stdout != nil {
		tty.Stderr = stdcopy.NewStdWriter(stdout, stdcopy.Stderr)
		tty.Stdout = stdcopy.NewStdWriter(stdout, stdcopy.Stdout)
	}

	go func(es *Exec) {
		result := p.sandbox.WaitProcess(false, []string{execId}, -1)
		if result == nil {
			err := fmt.Errorf("can not wait exec %s for container %s", execId, containerId)
			p.Log(ERROR, err)
			return
		}

		r, ok := <-result
		if !ok {
			err := fmt.Errorf("waiting exec %s of container %s interrupted", execId, containerId)
			p.Log(ERROR, err)
			return
		}

		p.Log(DEBUG, "exec %s of container %s terminated at %v with code %d", execId, containerId, r.FinishedAt, r.Code)
		es.ExitCode = r.Code
	}(es)

	return p.sandbox.Exec(es.Container, es.Id, es.Cmds, es.Terminal, tty)
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

	return es.ExitCode, nil
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
