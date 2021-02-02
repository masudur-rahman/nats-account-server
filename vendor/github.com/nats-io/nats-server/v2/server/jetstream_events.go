package server

import (
	"encoding/json"
	"time"
)

func (s *Server) publishAdvisory(acc *Account, subject string, adv interface{}) {
	ej, err := json.MarshalIndent(adv, "", "  ")
	if err == nil {
		err = s.sendInternalAccountMsg(acc, subject, ej)
		if err != nil {
			s.Warnf("Advisory could not be sent for account %q: %v", acc.Name, err)
		}
	} else {
		s.Warnf("Advisory could not be serialized for account %q: %v", acc.Name, err)
	}
}

// JSAPIAudit is an advisory about administrative actions taken on JetStream
type JSAPIAudit struct {
	TypedEvent
	Server   string      `json:"server"`
	Client   *ClientInfo `json:"client"`
	Subject  string      `json:"subject"`
	Request  string      `json:"request,omitempty"`
	Response string      `json:"response"`
}

const JSAPIAuditType = "io.nats.jetstream.advisory.v1.api_audit"

// ActionAdvisoryType indicates which action against a stream, consumer or template triggered an advisory
type ActionAdvisoryType string

const (
	CreateEvent ActionAdvisoryType = "create"
	DeleteEvent ActionAdvisoryType = "delete"
	ModifyEvent ActionAdvisoryType = "modify"
)

// JSStreamActionAdvisory indicates that a stream was created, edited or deleted
type JSStreamActionAdvisory struct {
	TypedEvent
	Stream   string             `json:"stream"`
	Action   ActionAdvisoryType `json:"action"`
	Template string             `json:"template,omitempty"`
}

const JSStreamActionAdvisoryType = "io.nats.jetstream.advisory.v1.stream_action"

// JSConsumerActionAdvisory indicates that a consumer was created or deleted
type JSConsumerActionAdvisory struct {
	TypedEvent
	Stream   string             `json:"stream"`
	Consumer string             `json:"consumer"`
	Action   ActionAdvisoryType `json:"action"`
}

const JSConsumerActionAdvisoryType = "io.nats.jetstream.advisory.v1.consumer_action"

// JSConsumerAckMetric is a metric published when a user acknowledges a message, the
// number of these that will be published is dependent on SampleFrequency
type JSConsumerAckMetric struct {
	TypedEvent
	Stream      string `json:"stream"`
	Consumer    string `json:"consumer"`
	ConsumerSeq uint64 `json:"consumer_seq"`
	StreamSeq   uint64 `json:"stream_seq"`
	Delay       int64  `json:"ack_time"`
	Deliveries  uint64 `json:"deliveries"`
}

// JSConsumerAckMetricType is the schema type for JSConsumerAckMetricType
const JSConsumerAckMetricType = "io.nats.jetstream.metric.v1.consumer_ack"

// JSConsumerDeliveryExceededAdvisory is an advisory informing that a message hit
// its MaxDeliver threshold and so might be a candidate for DLQ handling
type JSConsumerDeliveryExceededAdvisory struct {
	TypedEvent
	Stream     string `json:"stream"`
	Consumer   string `json:"consumer"`
	StreamSeq  uint64 `json:"stream_seq"`
	Deliveries uint64 `json:"deliveries"`
}

// JSConsumerDeliveryExceededAdvisoryType is the schema type for JSConsumerDeliveryExceededAdvisory
const JSConsumerDeliveryExceededAdvisoryType = "io.nats.jetstream.advisory.v1.max_deliver"

// JSConsumerDeliveryTerminatedAdvisory is an advisory informing that a message was
// terminated by the consumer, so might be a candidate for DLQ handling
type JSConsumerDeliveryTerminatedAdvisory struct {
	TypedEvent
	Stream      string `json:"stream"`
	Consumer    string `json:"consumer"`
	ConsumerSeq uint64 `json:"consumer_seq"`
	StreamSeq   uint64 `json:"stream_seq"`
	Deliveries  uint64 `json:"deliveries"`
}

// JSConsumerDeliveryTerminatedAdvisoryType is the schema type for JSConsumerDeliveryTerminatedAdvisory
const JSConsumerDeliveryTerminatedAdvisoryType = "io.nats.jetstream.advisory.v1.terminated"

// JSSnapshotCreateAdvisory is an advisory sent after a snapshot is successfully started
type JSSnapshotCreateAdvisory struct {
	TypedEvent
	Stream string      `json:"stream"`
	State  StreamState `json:"state"`
	Client *ClientInfo `json:"client"`
}

// JSSnapshotCreatedAdvisoryType is the schema type for JSSnapshotCreateAdvisory
const JSSnapshotCreatedAdvisoryType = "io.nats.jetstream.advisory.v1.snapshot_create"

// JSSnapshotCompleteAdvisory is an advisory sent after a snapshot is successfully started
type JSSnapshotCompleteAdvisory struct {
	TypedEvent
	Stream string      `json:"stream"`
	Start  time.Time   `json:"start"`
	End    time.Time   `json:"end"`
	Client *ClientInfo `json:"client"`
}

// JSSnapshotCompleteAdvisoryType is the schema type for JSSnapshotCreateAdvisory
const JSSnapshotCompleteAdvisoryType = "io.nats.jetstream.advisory.v1.snapshot_complete"

// JSRestoreCreateAdvisory is an advisory sent after a snapshot is successfully started
type JSRestoreCreateAdvisory struct {
	TypedEvent
	Stream string      `json:"stream"`
	Client *ClientInfo `json:"client"`
}

// JSRestoreCreateAdvisory is the schema type for JSSnapshotCreateAdvisory
const JSRestoreCreateAdvisoryType = "io.nats.jetstream.advisory.v1.restore_create"

// JSRestoreCompleteAdvisory is an advisory sent after a snapshot is successfully started
type JSRestoreCompleteAdvisory struct {
	TypedEvent
	Stream string      `json:"stream"`
	Start  time.Time   `json:"start"`
	End    time.Time   `json:"end"`
	Bytes  int64       `json:"bytes"`
	Client *ClientInfo `json:"client"`
}

// JSRestoreCompleteAdvisoryType is the schema type for JSSnapshotCreateAdvisory
const JSRestoreCompleteAdvisoryType = "io.nats.jetstream.advisory.v1.restore_complete"

// Clustering specific.

// JSStreamLeaderElectedAdvisoryType is sent when the system elects a leader for a stream.
const JSStreamLeaderElectedAdvisoryType = "io.nats.jetstream.advisory.v1.stream_leader_elected"

// JSStreamQuorumLostAdvisory indicates that a stream has lost quorum and is stalled.
type JSStreamLeaderElectedAdvisory struct {
	TypedEvent
	Account  string      `json:"account,omitempty"`
	Stream   string      `json:"stream"`
	Leader   string      `json:"leader"`
	Replicas []*PeerInfo `json:"replicas"`
}

// JSStreamQuorumAdvisoryType is sent when the system detects a clustered stream and
// its consumers are stalled and unable to make progress.
const JSStreamQuorumLostAdvisoryType = "io.nats.jetstream.advisory.v1.stream_quorum_lost"

// JSStreamQuorumLostAdvisory indicates that a stream has lost quorum and is stalled.
type JSStreamQuorumLostAdvisory struct {
	TypedEvent
	Account  string      `json:"account,omitempty"`
	Stream   string      `json:"stream"`
	Replicas []*PeerInfo `json:"replicas"`
}

// JSConsumerLeaderElectedAdvisoryType is sent when the system elects a leader for a consumer.
const JSConsumerLeaderElectedAdvisoryType = "io.nats.jetstream.advisory.v1.consumer_leader_elected"

// JSStreamQuorumLostAdvisory indicates that a stream has lost quorum and is stalled.
type JSConsumerLeaderElectedAdvisory struct {
	TypedEvent
	Account  string      `json:"account,omitempty"`
	Stream   string      `json:"stream"`
	Consumer string      `json:"consumer"`
	Leader   string      `json:"leader"`
	Replicas []*PeerInfo `json:"replicas"`
}

// JSConsumerQuorumAdvisoryType is sent when the system detects a clustered consumer and
// is stalled and unable to make progress.
const JSConsumerQuorumLostAdvisoryType = "io.nats.jetstream.advisory.v1.consumer_quorum_lost"

// JSConsumerQuorumLostAdvisory indicates that a consumer has lost quorum and is stalled.
type JSConsumerQuorumLostAdvisory struct {
	TypedEvent
	Account  string      `json:"account,omitempty"`
	Stream   string      `json:"stream"`
	Consumer string      `json:"consumer"`
	Replicas []*PeerInfo `json:"replicas"`
}