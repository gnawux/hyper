package daemon

import (
	"io"

	"github.com/docker/docker/pkg/stdcopy"
	"github.com/golang/glog"
)

func (daemon *Daemon) ExitCode(containerId, execId string) (int, error) {

	p, id, err := daemon.PodList.GetByContainerIdOrName(containerId)
	if err != nil {
		glog.Error(err)
		return 255, err
	}

	glog.V(1).Infof("Get Exec Code for container %s", containerId)

	code, err := p.GetExecExitCode(id, execId)
	return int(code), err
}

func (daemon *Daemon) CreateExec(containerId, cmd string, terminal bool) (string, error) {

	p, id, err := daemon.PodList.GetByContainerIdOrName(containerId)
	if err != nil {
		glog.Error(err)
		return "", err
	}

	glog.V(1).Infof("Create Exec for container %s", containerId)
	return p.CreateExec(id, cmd, terminal)
}

func (daemon *Daemon) StartExec(stdin io.ReadCloser, stdout io.WriteCloser, containerId, execId string) error {
	p, id, err := daemon.PodList.GetByContainerIdOrName(containerId)
	if err != nil {
		glog.Error(err)
		return "", err
	}

	glog.V(1).Infof("Start Exec for container %s", containerId)
	return p.StartExec(stdin, stdout, id, execId)
}
