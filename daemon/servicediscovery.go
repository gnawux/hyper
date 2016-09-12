package daemon

import (
	"fmt"

	"github.com/golang/glog"
	"github.com/hyperhq/hyperd/servicediscovery"
	"github.com/hyperhq/runv/hypervisor"
	"github.com/hyperhq/runv/hypervisor/pod"
)

func (daemon *Daemon) AddService(podId string, srvs []pod.UserService) error {
	vm, container, err := daemon.GetServiceContainerInfo(podId)
	if err != nil {
		return err
	}

	services, err := servicediscovery.GetServices(vm, container)
	if err != nil {
		return err
	}

	for _, s := range srvs {
		services = append(services, s)
	}

	if err := servicediscovery.ApplyServices(vm, container, services); err != nil {
		return err
	}

	return nil
}

func (daemon *Daemon) UpdateService(podId string, srvs []pod.UserService) error {
	vm, container, err := daemon.GetServiceContainerInfo(podId)
	if err != nil {
		return err
	}

	if err := servicediscovery.ApplyServices(vm, container, srvs); err != nil {
		return err
	}

	return nil
}

func (daemon *Daemon) DeleteService(podId string, srvs []pod.UserService) error {
	var services []pod.UserService
	var services2 []pod.UserService
	var found int = 0

	vm, container, err := daemon.GetServiceContainerInfo(podId)
	if err != nil {
		return err
	}

	services, err = servicediscovery.GetServices(vm, container)
	if err != nil {
		return err
	}

	for _, s := range services {
		shouldRemain := true
		for _, srv := range srvs {
			if s.ServiceIP == srv.ServiceIP && s.ServicePort == srv.ServicePort {
				shouldRemain = false
				found = 1
				break
			}
		}

		if shouldRemain {
			services2 = append(services2, s)
		}
	}

	if found == 0 {
		return fmt.Errorf("Pod %s doesn't use this service", podId)
	}

	if err := servicediscovery.ApplyServices(vm, container, services2); err != nil {
		return err
	}

	return nil
}

func (daemon *Daemon) GetServices(podId string) ([]pod.UserService, error) {
	vm, container, err := daemon.GetServiceContainerInfo(podId)
	if err != nil {
		return nil, err
	}

	services, err := servicediscovery.GetServices(vm, container)
	if err != nil {
		return nil, err
	}

	return services, nil
}

func (daemon *Daemon) GetServiceContainerInfo(podId string) (*hypervisor.Vm, string, error) {
	pod, ok := daemon.PodList.Get(podId)
	if !ok {
		return nil, "", fmt.Errorf("Cannot find Pod %s", podId)
	}

	if pod.PodStatus.Type != "service-discovery" || len(pod.PodStatus.Containers) <= 1 {
		return nil, "", fmt.Errorf("Pod %s doesn't have services discovery", podId)
	}

	container := pod.PodStatus.Containers[0].Id
	glog.V(1).Infof("Get container id is %s", container)

	if pod.VM == nil {
		return nil, "", fmt.Errorf("Cannot find VM for %s!", podId)
	}

	return pod.VM, container, nil
}
