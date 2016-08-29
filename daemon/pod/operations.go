package pod

import (
	"fmt"
	"syscall"

	"github.com/golang/glog"
)

func (p *Pod) Kill(id string, sig int64) error {
	return p.sandbox.KillContainer(id, syscall.Signal(sig))
}

func (p *Pod) ForceQuit() {
	p.sandbox.Kill()
}

func (p *Pod) Pause() error {
	p.status.lock.Lock()
	defer p.status.lock.Unlock()

	if p.status.pod != S_POD_RUNNING || p.sandbox == nil {
		err := fmt.Errorf("%s is not running, cannot pause", p.Name)
		glog.Error(err)
		return err
	}

	err := p.sandbox.Pause(true)
	if err != nil {
		glog.Error(err)
		return err
	}

	p.status.pod = S_POD_PAUSING
	return nil
}

func (p *Pod) UnPause() error {
	p.status.lock.Lock()
	defer p.status.lock.Unlock()

	if p.status.pod != S_POD_PAUSING || p.sandbox == nil {
		err := fmt.Errorf("%s is not paused, cannot resume", p.Name)
		glog.Error(err)
		return err
	}

	err := p.sandbox.Pause(false)
	if err != nil {
		glog.Error(err)
		return err
	}

	p.status.pod = S_POD_RUNNING
	return nil
}
