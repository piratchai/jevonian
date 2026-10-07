package chatgptweb

import (
	"sync"
)

// TurnEvidence is a process-local record of the last completed turn's model
// correlation. An end-to-end verification harness that runs the server
// in-process can read it to prove that the model the client asked for is the
// model ID that ChatGPT actually received in the outbound conversation POST.
type TurnEvidence struct {
	// RequestedModel is the catalog model ID the client selected.
	RequestedModel string
	// ObservedModel is the model ID seen in the outbound ChatGPT request body.
	ObservedModel string
	// Correlated is true when a unique outbound request was observed and its
	// model satisfied the requested model.
	Correlated bool
	// Streaming is true for a streaming turn.
	Streaming bool
}

var (
	turnEvidenceMu sync.Mutex
	turnEvidence   TurnEvidence
)

// RecordTurnEvidence stores the model correlation of the last completed turn.
func RecordTurnEvidence(ev TurnEvidence) {
	turnEvidenceMu.Lock()
	turnEvidence = ev
	turnEvidenceMu.Unlock()
}

// LastTurnEvidence returns the model correlation of the last completed turn.
func LastTurnEvidence() TurnEvidence {
	turnEvidenceMu.Lock()
	defer turnEvidenceMu.Unlock()
	return turnEvidence
}
