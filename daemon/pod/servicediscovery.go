package pod

import (
	"fmt"
	"path"

	"github.com/hyperhq/hyperd/servicediscovery"
	apitypes "github.com/hyperhq/hyperd/types"
	"github.com/hyperhq/hyperd/utils"
)

func ParseServiceDiscovery(id string, spec *apitypes.UserPod) *apitypes.UserContainer {
	var serviceDir string = path.Join(utils.HYPER_ROOT, "services", id)
	var serviceType string = "service-discovery"

	if len(spec.Services) == 0 || spec.Type == serviceType {
		return nil
	}

	spec.Type = serviceType
	/* PrepareServices will check service volume */
	serviceVolume := &apitypes.UserVolume{
		Name:   "service-volume",
		Source: serviceDir,
		Format: "vfs",
		Fstype: "dir",
	}

	serviceVolRef := &apitypes.UserVolumeReference{
		Volume:   "service-volume",
		Path:     servicediscovery.ServiceVolume,
		ReadOnly: false,
		Detail:   serviceVolume,
	}

	return &apitypes.UserContainer{
		Name:    ServiceDiscoveryContainerName(spec.Id),
		Image:   servicediscovery.ServiceImage,
		Command: []string{"haproxy", "-D", "-f", "/usr/local/etc/haproxy/haproxy.cfg", "-p", "/var/run/haproxy.pid"},
		Volumes: []*apitypes.UserVolumeReference{serviceVolRef},
		Type:    apitypes.UserContainer_SERVICE,
	}
}

func ServiceDiscoveryContainerName(podName string) string {
	return podName + "-" + utils.RandStr(10, "alpha") + "-service-discovery"
}

func (p *XPod) GetServices() ([]*apitypes.UserService, error) {
	return p.services, nil
}

func (p *XPod) UpdateService(srvs []*apitypes.UserService) error {
	if p.globalSpec.Type != "service-discovery" {
		err := fmt.Errorf("pod does not support service discovery")
		p.Log(ERROR, err)
		return err
	}
	p.resourceLock.Lock()
	defer p.resourceLock.Unlock()

	if p.IsRunning() {
		sc := p.getServiceContainer()
		if err := servicediscovery.ApplyServices(p.sandbox, sc, srvs); err != nil {
			p.Log(ERROR, "failed to update services %#v: %v", srvs, err)
			return err
		}
	}
	p.services = srvs
	return nil
}

func (p *XPod) AddService(srvs []*apitypes.UserService) error {
	if p.globalSpec.Type != "service-discovery" {
		err := fmt.Errorf("pod does not support service discovery")
		p.Log(ERROR, err)
		return err
	}
	p.resourceLock.Lock()
	defer p.resourceLock.Unlock()

	target := append(p.services, srvs...)

	if p.IsRunning() {
		sc := p.getServiceContainer()
		if err := servicediscovery.ApplyServices(p.sandbox, sc, target); err != nil {
			p.Log(ERROR, "failed to update services %#v: %v", target, err)
			return err
		}
	}
	p.services = target
	return nil
}

func (p *XPod) DeleteService(srvs []*apitypes.UserService) error {
	if p.globalSpec.Type != "service-discovery" {
		err := fmt.Errorf("pod does not support service discovery")
		p.Log(ERROR, err)
		return err
	}
	p.resourceLock.Lock()
	defer p.resourceLock.Unlock()

	tbd := make(map[struct {
		IP   string
		Port int32
	}]bool, len(srvs))
	for _, srv := range srvs {
		tbd[struct {
			IP   string
			Port int32
		}{srv.ServiceIP, srv.ServicePort}] = true
	}
	target := make([]*apitypes.UserService, len(p.services))
	for _, srv := range p.services {
		if tbd[struct {
			IP   string
			Port int32
		}{srv.ServiceIP, srv.ServicePort}] {
			p.Log(TRACE, "remove service %#v", srv)
			continue
		}
		target = append(target, srv)
	}

	if p.IsRunning() {
		sc := p.getServiceContainer()
		if err := servicediscovery.ApplyServices(p.sandbox, sc, target); err != nil {
			p.Log(ERROR, "failed to update services %#v: %v", target, err)
			return err
		}
	}
	p.services = target
	return nil
}

func (p *XPod) getServiceContainer() string {
	if p.globalSpec.Type != "service-discovery" {
		return ""
	}
	for _, c := range p.containers {
		if c.spec.Type == apitypes.UserContainer_SERVICE {
			return c.SpecName()
		}
	}
	return ""
}
