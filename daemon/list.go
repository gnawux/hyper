package daemon

import (
	"fmt"

	"github.com/hyperhq/hyperd/daemon/pod"
	apitypes "github.com/hyperhq/hyperd/types"
)

type pMatcher func(p *pod.XPod) (match, quit bool)

func (daemon *Daemon) snapshotPodList(podId, vmId string) []*pod.XPod {
	var (
		pl = []*pod.XPod{}
	)

	if podId != "" {
		p, ok := daemon.PodList.Get(podId)
		if ok {
			pl = append(pl, p)
		}
		if vmId != "" && p.SandboxName() != vmId {
			return []*pod.XPod{}
		}
		return pl
	}

	if vmId != "" {
		p := daemon.PodList.Find(func(p *pod.XPod) bool {
			return p.SandboxName() == vmId
		})
		if p != nil {
			pl = append(pl, p)
		}
		return pl
	}

	daemon.PodList.Foreach(func(p *pod.XPod) error {
		pl = append(pl, p)
		return nil
	})
	return pl
}

func (daemon *Daemon) ListContainers(podId, vmId string, auxiliary bool) ([]*apitypes.ContainerListResult, error) {
	var (
		cids   []string
		result = []*apitypes.ContainerListResult{}
	)
	pl := daemon.snapshotPodList(podId, vmId)
	for _, p := range pl {
		if auxiliary {
			cids = p.ContainerIds()
		} else {
			cids = p.ContainerIdsOf(apitypes.UserContainer_REGULAR)
		}
		for _, cid := range cids {
			status := p.ContainerBriefStatus(cid)
			if status != nil {
				result = append(result, status)
			}
		}

	}
	return result, nil
}

func (daemon *Daemon) ListPods(podId, vmId string) ([]*apitypes.PodListResult, error) {
	pl := daemon.snapshotPodList(podId, vmId)
	result := make([]*apitypes.PodListResult, 0, len(pl))
	for _, p := range pl {
		if s := p.BriefStatus(); s != nil {
			result = append(result, s)
		}
	}
	return result, nil
}

func (daemon *Daemon) ListVMs(podId, vmId string) ([]*apitypes.VMListResult, error) {
	pl := daemon.snapshotPodList(podId, vmId)
	result := make([]*apitypes.VMListResult, 0, len(pl))
	for _, p := range pl {
		if s := p.SandboxBriefStatus(); s != nil {
			result = append(result, s)
		}
	}

	return result, nil
}

func (daemon *Daemon) List(item, podId, vmId string, auxiliary bool) (map[string][]string, error) {
	var (
		pl       = []*pod.XPod{}
		matchers = []pMatcher{}

		list                  = make(map[string][]string)
		vmJsonResponse        = []string{}
		podJsonResponse       = []string{}
		containerJsonResponse = []string{}
	)
	if item != "pod" && item != "container" && item != "vm" {
		return list, fmt.Errorf("Can not support %s list!", item)
	}

	if vmId != "" {
		m := func(p *pod.XPod) (match, quit bool) {
			if p.SandboxName() == vmId {
				return true, true
			}
			return false, false
		}
		matchers = append(matchers, m)
	}

	if podId != "" {
		p, ok := daemon.PodList.Get(podId)
		if ok {
			pl = append(pl, p)
		}
	}

	if len(pl) > 0 {
		xpl := pl
		pl = []*pod.XPod{}

		if len(matchers) > 0 {
			for _, p := range xpl {
				var (
					match = true
					quit  = false
				)
				for _, matcher := range matchers {
					m, q := matcher(p)
					match = match && m
					quit = quit || q
				}
				if match {
					pl = append(pl, p)
				}
				if quit {
					break
				}
			}
		}
	} else if len(matchers) == 0 {
		daemon.PodList.Foreach(func(p *pod.XPod) error {
			pl = append(pl, p)
			return nil
		})
	} else {
		daemon.PodList.Find(func(p *pod.XPod) bool {
			var (
				match = true
				quit  = false
			)
			for _, matcher := range matchers {
				m, q := matcher(p)
				match = match && m
				quit = quit || q
			}
			if match {
				pl = append(pl, p)
			}
			return quit
		})
	}

	for _, p := range pl {
		if p.IsNone() {
			p.Log(pod.TRACE, "listing: ignore none status pod")
			continue
		}
		switch item {
		case "vm":
			vm := p.SandboxName()
			if vm == "" {
				continue
			}
			vmJsonResponse = append(vmJsonResponse, p.SandboxStatusString())
		case "pod":
			podJsonResponse = append(podJsonResponse, p.StatusString())
		case "container":
			var cids []string
			if auxiliary {
				cids = p.ContainerIds()
			} else {
				cids = p.ContainerIdsOf(apitypes.UserContainer_REGULAR)
			}
			for _, cid := range cids {
				status := p.ContainerStatusString(cid)
				if status != "" {
					containerJsonResponse = append(containerJsonResponse, status)
				}
			}
		}
	}

	switch item {
	case "vm":
		list["vmData"] = vmJsonResponse
	case "pod":
		list["podData"] = podJsonResponse
	case "container":
		list["cData"] = containerJsonResponse
	}

	return list, nil
}
