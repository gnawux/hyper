package pod

import (
	"fmt"
	"io"

	"github.com/docker/docker/pkg/stdcopy"
	"github.com/golang/glog"

	"github.com/hyperhq/hyperd/utils"
	"github.com/hyperhq/runv/hypervisor"
	"github.com/hyperhq/runv/hypervisor/types"
)

func (p *Pod) CreateExec(containerId, cmd string, terminal bool) (string, error) {
	cs, ok := p.status.containers[containerId]
	if !ok {
		err := fmt.Errorf("no container %s available for exec %s", containerId, cmd)
		glog.Error(err)
		return "", err
	}

	if !cs.IsAlive() {
		err := fmt.Errorf("container %s is not available (%d) for exec %s", containerId, cs.CurrentState(), cmd)
		glog.Error(err)
		return "", err
	}

	execId := fmt.Sprintf("exec-%s", utils.RandStr(10, "alpha"))

	p.status.lock.Lock()
	defer p.status.lock.Unlock()

	p.status.AddExec(containerId, execId, cmd, terminal)
	return execId, nil
}

func (p *Pod) StartExec(stdin io.ReadCloser, stdout io.WriteCloser, containerId, execId string) error {
	p.status.lock.RLock()
	defer p.status.lock.RUnlock()

	es := p.status.GetExec(execId)
	if es == nil {
		err := fmt.Errorf("no exec %s exists for container %s", execId, containerId)
		glog.Error(err)
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

	go func() {
		result := p.sandbox.WaitProcess(false, []string{execId}, -1)
		if result == nil {
			err := fmt.Errorf("can not wait exec %s for container %s", execId, containerId)
			glog.Error(err)
			return
		}

		r, ok := <- result
		if !ok {
			err := fmt.Errorf("waiting exec %s of container %s interrupted", execId, containerId)
			glog.Error(err)
			return
		}

		p.status.lock.RLock()
		defer p.status.lock.RUnlock()

		glog.V(1).Infof("exec %s of container %s terminated at %v with code %d", execId, containerId, r.FinishedAt, r.Code)
		p.status.SetExecStatus(execId, r.Code)
	}()

	return p.sandbox.Exec(es.Container, es.Id, es.Cmds, es.Terminal, tty)
}

func (p *Pod) GetExecExitCode(containerId, execId string) (uint8, error) {
	p.status.lock.RLock()
	defer p.status.lock.RUnlock()

	es := p.status.GetExec(execId)
	if es == nil {
		err := fmt.Errorf("no exec %s exists for container %s", execId, containerId)
		glog.Error(err)
		return 255, err
	}

	return es.ExitCode, nil
}

func (p *Pod) DeleteExec(containerId, execId string) {
	p.status.lock.Lock()
	defer p.status.lock.Unlock()

	p.status.DeleteExec(execId)
}
