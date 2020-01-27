package state

import (
	"sort"

	"github.com/hashicorp/consul/agent/consul/stream"
	"github.com/hashicorp/consul/agent/structs"
	"github.com/hashicorp/consul/api"
	memdb "github.com/hashicorp/go-memdb"
)

// txnServiceHealthEvents returns the list of ServiceHealth events generated by
// the given Txn ops.
func txnServiceHealthEvents(s *Store, tx *memdb.Txn, idx uint64, ops structs.TxnOps) ([]stream.Event, error) {
	// Collect the list of nodes to generate ServiceHealth events for.
	// Store each node in a map to de-dup so we don't send CheckServiceNode
	// updates more than once. True denotes a deregister operation. We run through
	// the transaction in order so if the last op is a delete on the node, only
	// a deregister should be sent.
	nodes := make(map[string]bool)
	for _, op := range ops {
		switch {
		case op.Node != nil:
			if op.Node.Verb == api.NodeGet {
				continue
			}
			nodes[op.Node.Node.Node] = op.Node.Verb == api.NodeDelete || op.Node.Verb == api.NodeDeleteCAS
		case op.Service != nil:
			if op.Service.Verb == api.ServiceGet {
				continue
			}
			nodes[op.Service.Node] = false
		case op.Check != nil:
			if op.Check.Verb == api.CheckGet {
				continue
			}
			nodes[op.Check.Check.Node] = false
		default:
		}
	}

	var events []stream.Event
	for node, deregister := range nodes {
		// If this node is being deregistered, skip the lookup.
		if deregister {
			events = append(events, serviceHealthDeregisterEvent(idx, node))
			continue
		}

		evt, err := nodeServiceHealth(s, tx, idx, node)
		if err != nil {
			return nil, err
		}

		events = append(events, evt...)
	}

	sort.Slice(events, func(i, j int) bool {
		a := events[i].GetServiceHealth().CheckServiceNode
		b := events[j].GetServiceHealth().CheckServiceNode
		if a.Node.Node != b.Node.Node {
			return a.Node.Node < b.Node.Node
		}

		var svcA, svcB string
		if a.Service != nil {
			svcA = a.Service.Service
		}
		if b.Service != nil {
			svcB = b.Service.Service
		}
		return svcA < svcB
	})

	return events, nil
}

// nodeServiceHealth returns a ServiceHealth event for a single node.
func nodeServiceHealth(s *Store, tx *memdb.Txn, idx uint64, nodeName string) ([]stream.Event, error) {
	_, services, err := s.nodeServicesTxn(tx, nil, nodeName, "", false)
	if err != nil {
		return nil, err
	}
	_, nodes, err := s.parseCheckServiceNodes(tx, nil, idx, "", services, nil)
	if err != nil {
		return nil, err
	}

	events := checkServiceNodesToServiceHealth(idx, nodes, nil, false)
	return events, nil
}

// checkServiceNodesToServiceHealth converts a list of CheckServiceNodes to
// ServiceHealth events for streaming. If a non-nil event buffer is passed,
// events are appended to the buffer one at a time and an empty slice is
// returned to avoid keeping a full copy in memory.
func checkServiceNodesToServiceHealth(idx uint64, nodes structs.CheckServiceNodes,
	buf *stream.EventBuffer, connect bool) []stream.Event {
	var events []stream.Event
	for _, n := range nodes {
		event := stream.Event{
			Index: idx,
		}

		if connect {
			event.Topic = stream.Topic_ServiceHealthConnect
		} else {
			event.Topic = stream.Topic_ServiceHealth
		}

		if n.Service != nil {
			event.Key = n.Service.Service
		}

		event.Payload = &stream.Event_ServiceHealth{
			ServiceHealth: &stream.ServiceHealthUpdate{
				Op:               stream.CatalogOp_Register,
				CheckServiceNode: stream.ToCheckServiceNode(&n),
			},
		}
		if buf != nil {
			buf.Append([]stream.Event{event})
		} else {
			events = append(events, event)
		}
	}
	return events
}

// serviceHealthDeregisterEvent returns a ServiceHealth event for deregistering
// the given node.
func serviceHealthDeregisterEvent(idx uint64, node string) stream.Event {
	return stream.Event{
		Topic: stream.Topic_ServiceHealth,
		Index: idx,
		Payload: &stream.Event_ServiceHealth{
			ServiceHealth: &stream.ServiceHealthUpdate{
				Op: stream.CatalogOp_Deregister,
				CheckServiceNode: &stream.CheckServiceNode{
					Node: &stream.Node{Node: node},
				},
			},
		},
	}
}