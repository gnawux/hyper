package daemon

import (
	"fmt"

	"github.com/hyperhq/hyperd/daemon/pod"
	apitypes "github.com/hyperhq/hyperd/types"
)

type pMatcher func(p *pod.XPod) (match, quit bool)

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
		append(matchers, m)
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
					quit  = quit || q
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
		daemon.PodList.Foreach(func(p *pod.XPod) {
			pl = append(pl, p)
		})
	} else {
		daemon.PodList.Find(func(p *pod.XPod) {
			var (
				match = true
				quit  = false
			)
			for _, matcher := range matchers {
				m, q := matcher(p)
				match = match && m
				quit  = quit || q
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
