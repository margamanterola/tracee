package ebpf

import (
	gocontext "context"
	"sync"

	"github.com/aquasecurity/tracee/pkg/containers/runtime"
	"github.com/aquasecurity/tracee/pkg/events"
	"github.com/aquasecurity/tracee/pkg/events/parse"
	"github.com/aquasecurity/tracee/types/trace"
)

//
// Producer:
//
// Each cgroupId gets its own channel of events. Events are enqueued and only
// de-dequed once the cgroupId was enriched OR has failed enrichment.
//
// [cgroupId #A]: queue of trace events for container described by cgroupId #A
// [cgroupId #B]: queue of trace events for container described by cgroupId #B
// [cgroupId #C]: queue of trace events for container described by cgroupId #C
// ...
//
// If the cgroupId channel does not exist it is created and an enrichment phase
// is triggered for that cgroupId. If the cgroup channel is not used after a
// specific period, it is removed.
//
// Consumer:
//
// In order to de-queue an event from its queue there is a scheduled operation
// to each queued event.
//
// queueReady: it is a simple scheduler of events using their cgroupId as index
//
// [cgroupId #A][cgroupId #B][cgroupId #C][#cgroupId #B][cgroupId #C]...
//
// One event is de-queued from queueReady only if its respective cgroupId has
// been enriched OR has failed enrichment.
//
// Observation:
//
// With this model, the pipeline will only block when one of these channels is
// full. In this case, pipeline will be blocked until this channel's cgroupId
// is enriched and its enqueued events are de-queued.
//

func (t *Tracee) enrichContainerEvents(ctx gocontext.Context, in <-chan *trace.Event) (chan *trace.Event, chan error) {
	const (
		contQueueSize  = 10000  // max num of events queued per container
		queueReadySize = 100000 // max num of events queued in total
	)

	type enrichResult struct {
		result runtime.ContainerMetadata
		err    error
	}

	// big lock
	bLock := sync.RWMutex{}
	// pipeline channels
	out := make(chan *trace.Event, 10000)
	errc := make(chan error, 1)
	// state machine for enrichment
	enrichDone := make(map[uint64]bool)
	enrichInfo := make(map[uint64]*enrichResult)
	// 1 queue per cgroupId
	queues := make(map[uint64]chan *trace.Event)
	// scheduler queue
	queueReady := make(chan uint64, queueReadySize)

	// queues map writer
	go func() {
		defer close(out)
		defer close(errc)
		for { // enqueue events
			select {
			case event := <-in:
				// send out irrelevant events
				if event.ContainerID == "" {
					out <- event
					continue
				}
				cgroupId := uint64(event.CgroupID)
				// CgroupMkdir: pick EventID from the event itself
				if event.EventID == int(events.CgroupMkdir) {
					cgroupId, _ = parse.ArgUint64Val(event, "cgroup_id")
				}
				// CgroupRmdir: clean up remaining events and maps
				if event.EventID == int(events.CgroupRmdir) {
					var eInfo *enrichResult
					cgroupId, _ = parse.ArgUint64Val(event, "cgroup_id")
					bLock.RLock()
					if i, ok := enrichInfo[cgroupId]; ok {
						eInfo = i
					}
					for evt := range queues[cgroupId] {
						if evt.ContainerImage == "" && eInfo != nil {
							bLock.RUnlock()
							enrichEvent(evt, eInfo.result)
							bLock.RLock()
						}
						out <- evt
					}
					bLock.RUnlock()
					bLock.Lock()
					delete(enrichDone, cgroupId)
					delete(enrichInfo, cgroupId)
					delete(queues, cgroupId)
					bLock.Unlock()
					continue
				}
				// make sure a queue channel exists for this cgroupId
				bLock.Lock()
				if _, ok := queues[cgroupId]; !ok {
					queues[cgroupId] = make(chan *trace.Event, contQueueSize)

					go func(cgroupId uint64) {
						metadata, err := t.containers.EnrichCgroupInfo(cgroupId)
						bLock.Lock()
						enrichInfo[cgroupId] = &enrichResult{metadata, err}
						enrichDone[cgroupId] = true
						bLock.Unlock()
					}(cgroupId)
				}
				bLock.Unlock() // give parallel enrichment routine a chance!
				bLock.RLock()
				// enqueue the event and schedule the operation
				queues[cgroupId] <- event
				bLock.RUnlock()
				queueReady <- cgroupId
			case <-ctx.Done():
				return
			}
		}
	}()

	// queues map reader
	go func() {
		for { // de-queue events
			select {
			case cgroupId := <-queueReady: // queue for received cgroupId is ready
				bLock.RLock()
				if !enrichDone[cgroupId] {
					// re-schedule the operation if queue is not enriched
					queueReady <- cgroupId
				} else {
					// de-queue event if queue is enriched
					if _, ok := queues[cgroupId]; ok {
						event := <-queues[cgroupId]
						if event.ContainerImage == "" {
							// event is not enriched: enrich if enrichment worked
							i := enrichInfo[cgroupId]
							if i.err == nil {
								enrichEvent(event, i.result)
							}
						}
						out <- event
					} // TODO: place a unlikely to happen error in the printer
				}
				bLock.RUnlock()
			// cleanup
			case <-ctx.Done():
				return
			}
		}
	}()

	return out, errc
}

func enrichEvent(evt *trace.Event, enrichData runtime.ContainerMetadata) {
	evt.ContainerImage = enrichData.Image
	evt.ContainerName = enrichData.Name
	evt.PodName = enrichData.Pod.Name
	evt.PodNamespace = enrichData.Pod.Namespace
	evt.PodUID = enrichData.Pod.UID
}