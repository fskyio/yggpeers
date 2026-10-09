// Package protocol defines the persistence-independent probe wire contract.
package protocol

import "time"

type Status string

const (
	Up      Status = "up"
	Down    Status = "down"
	Unknown Status = "unknown"

	CheckInPath = "/api/probe/v1/check-in"
	ResultsPath = "/api/probe/v1/results"
)

func (s Status) Valid() bool { return s == Up || s == Down || s == Unknown }

type Target struct {
	PeerID    int64  `json:"peer_id"`
	URI       string `json:"uri"`
	Protocol  string `json:"protocol"`
	Host      string `json:"host"`
	Port      string `json:"port"`
	IsNetwork bool   `json:"is_network"`
}

type CheckInRequest struct {
	CheckDarknet bool `json:"check_darknet"`
}

type Assignment struct {
	RoundID   int64     `json:"round_id"`
	StartedAt time.Time `json:"started_at"`
	Deadline  time.Time `json:"deadline"`
	Targets   []Target  `json:"targets"`
}

type WorkResponse struct {
	ProbeID   string      `json:"probe_id"`
	PollAfter int64       `json:"poll_after_ms"`
	Work      *Assignment `json:"work"`
}

type Result struct {
	PeerID int64  `json:"peer_id"`
	Status Status `json:"status"`
}

type ReportRequest struct {
	RoundID int64    `json:"round_id"`
	Results []Result `json:"results"`
}

type ReportResponse struct {
	Accepted int `json:"accepted"`
}
