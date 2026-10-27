package core

// FiringAlert is an active alert stamped with when it started firing (epoch ms).
type FiringAlert struct {
	Alert
	Since int64 `json:"since"`
}

type ServiceOperation struct {
	Operation string `json:"operation"`
	Calls     int    `json:"calls"`
	Errors    int    `json:"errors"`
	AvgMicros int64  `json:"avgMicros"`
}

// ServiceDetail is the per-service view of the live window: dependencies and own operations.
type ServiceDetail struct {
	Name       string             `json:"name"`
	Spans      int                `json:"spans"`
	Errors     int                `json:"errors"`
	ErrorRate  float64            `json:"errorRate"`
	Inbound    []string           `json:"inbound"`
	Outbound   []string           `json:"outbound"`
	Operations []ServiceOperation `json:"operations"`
}
