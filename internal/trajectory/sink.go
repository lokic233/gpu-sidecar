package trajectory

import "github.com/lokic233/gpu-sidecar/internal/dataplane"

// SidecarSink adapts the trajectory Emitter to dataplane.EventSink.
type SidecarSink struct{ E *Emitter }

func (s SidecarSink) Emit(ev dataplane.LocalEvent) {
	s.E.Emit(Event{
		Kind: ev.Kind, RequestID: ev.RequestID, RouteID: ev.RouteID, BackendID: ev.BackendID,
		HostID: ev.HostID, DeviceID: ev.DeviceID, Wall: ev.Wall, Fields: ev.Fields,
	})
}
