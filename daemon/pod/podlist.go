package pod

import (
	"fmt"
	"strings"
	"sync"
)

type PodList struct {
	pods       map[string]*Pod
	containers map[string]string
	containerNames map[string]string
	mu         *sync.RWMutex
}

func NewPodList() *PodList {
	return &PodList{
		pods:       make(map[string]*Pod),
		containers: make(map[string]string),
		containerNames: make(map[string]string),
		mu:         &sync.RWMutex{},
	}
}

func (pl *PodList) Get(id string) (*Pod, bool) {
	pl.mu.RLock()
	defer pl.mu.RUnlock()

	p, ok := pl.pods[id]
	return p, ok
}

func (pl *PodList) PutContainerID(id, pod string) error {
	if pn, ok := pl.containers[id]; ok && pn !=pod {
		return fmt.Errorf("the container id %s has already taken by pod %s", id, pn)
	}
	pl.containers[id] = pod
	return nil
}

func (pl *PodList) PutContainer(id, name, pod string) (chan<- bool, error) {
	pl.mu.Lock()
	defer pl.mu.Unlock()
	if _, ok := pl.pods[pod]; !ok {
		return nil, fmt.Errorf("pod %s not exist for adding container %s(%s)", pod, name, id)
	}
	if pn, ok := pl.containerNames[name]; ok && pn != pod {
		return nil,fmt.Errorf("container name %s has already taken by pod %s", name, pn)
	}
	if id != "" {
		if pn, ok := pl.containers[id]; ok && pn !=pod {
			return nil, fmt.Errorf("the container id %s has already taken by pod %s", id, pn)
		}
		pl.containers[id] = pod
	}
	pl.containerNames[name] = pod

	confirm := make(chan bool, 1)
	go func() {
		yes, ok := <-confirm
		if yes && ok {
			return
		}
		// if not confirmed, release the names
		pl.mu.Lock()
		defer pl.mu.Unlock()
		delete(pl.containerNames, name)
		delete(pl.containers, id)
	}()
	return confirm, nil
}

func (pl *PodList) PutPod(p *Pod) (chan<- bool, error) {
	pl.mu.Lock()
	defer pl.mu.Unlock()

	names := []string{}
	ids := []string{}

	name := p.Name
	// check availability
	if _, ok := pl.pods[name]; ok {
		return nil, fmt.Errorf("pod name %s has already in use", p.Name)
	}

	for _, c := range p.spec.Containers {
		if pn, ok := pl.containerNames[c.Name]; ok && pn != p.Name {
			return nil, fmt.Errorf("container name %s has already taken by pod %s", c.Name, pn)
		}
		if c.Id != "" {
			if pn, ok :=pl.containers[c.Id]; ok && pn != p.Name {
				return nil, fmt.Errorf("the container id %s has already taken by pod %s", c.Id, pn)
			}
			ids = append(ids, c.Id)
		}
		names = append(names, c.Name)
	}

	for _, n := range names {
		pl.containerNames[n] = p.Name
	}
	for _, i := range ids {
		pl.containers[i] = p.Name
	}

	confirm := make(chan bool, 1)
	go func() {
		yes, ok := <-confirm
		if yes && ok {
			return
		}
		// if not confirmed, release the names
		pl.mu.Lock()
		defer pl.mu.Unlock()
		for _, n := range names {
			delete(pl.containerNames, n)
		}
		for _, i := range ids {
			delete(pl.containers, i)
		}
		delete(pl.pods, name)
	}()
	return confirm, nil
}

func (pl *PodList) Delete(id string) {
	pl.mu.Lock()
	defer pl.mu.Unlock()
	if p, ok := pl.pods[id]; ok {
		for _, c := range p.spec.Containers {
			delete(pl.containers, c.Id)
			delete(pl.containerNames, c.Name)
		}
	}
	delete(pl.pods, id)
}

func (pl *PodList) DeleteContainer(id, name string) {
	pl.mu.Lock()
	defer pl.mu.Unlock()

	delete(pl.containers, id)
	delete(pl.containerNames, name)
}

func (pl *PodList) GetByContainerId(cid string) (*Pod, bool) {
	pl.mu.RLock()
	defer pl.mu.RUnlock()

	if podid, ok := pl.containers[cid]; ok {
		p, ok := pl.pods[podid]
		return p, ok
	}
	return nil, false
}

func (pl *PodList) GetByContainerIdOrName(cid string) (*Pod, string, bool) {
	pl.mu.RLock()
	defer pl.mu.RUnlock()

	if podid, ok := pl.containerNames[cid]; ok {
		if p, ok := pl.pods[podid]; ok {
			id, _ := p.ContainerName2Id(cid)
			return p, id,  true
		}
		return nil, "", false
	}
	if podid, ok := pl.containers[cid]; ok {
		if p, ok := pl.pods[podid]; ok {
			return p, cid, true
		}
		return nil, "", false
	}

	matchPods := []string{}
	fullid := ""
	for c, p := range pl.containers {
		if strings.HasPrefix(c, cid) {
			fullid = c
			matchPods = append(matchPods, p)
		}
	}
	if len(matchPods) > 1 {
		return nil, "", false
	} else if len(matchPods) == 1 {
		if p, ok := pl.pods[matchPods[0]]; ok {
			return p, fullid, true
		}
		return nil, "", false
	}

	return nil, "", false
}

func (pl *PodList) CountRunning() int64 {
	return pl.CountStatus(S_POD_RUNNING) + pl.CountStatus(S_POD_CREATING)
}

func (pl *PodList) CountAll() int64 {
	return int64(len(pl.pods))
}

func (pl *PodList) CountStatus(status uint) (num int64) {
	num = 0

	pl.mu.RLock()
	defer pl.mu.RUnlock()

	if pl.pods == nil {
		return
	}

	for _, pod := range pl.pods {
		if pod.status.pod == status {
			num++
		}
	}

	return
}

func (pl *PodList) CountContainers() (num int64) {
	num = 0
	pl.mu.RLock()
	defer pl.mu.RUnlock()

	return int64(len(pl.containers))
}

type PodOp func(*Pod) error
type PodFilterOp func(*Pod) bool

func (pl *PodList) Foreach(fn PodOp) error {
	pl.mu.RLock()
	defer pl.mu.RUnlock()
	return pl.foreachUnsafe(fn)
}

func (pl *PodList) foreachUnsafe(fn PodOp) error {
	for _, p := range pl.pods {
		if err := fn(p); err != nil {
			return err
		}
	}
	return nil
}

func (pl *PodList) Find(fn PodFilterOp) *Pod {
	pl.mu.RLock()
	defer pl.mu.RUnlock()
	return pl.findUnsafe(fn)
}

func (pl *PodList) findUnsafe(fn PodFilterOp) *Pod {
	for _, p := range pl.pods {
		if fn(p) {
			return p
		}
	}
	return nil
}
