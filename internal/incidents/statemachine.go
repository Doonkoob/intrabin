// Package incidents implements the persistent incident state machine.
package incidents

import (
	"fmt"

	"github.com/intrabin/intrabin/internal/model"
)

type S = model.IncidentStatus

// transitions lists allowed incident state transitions. RESOLVED → SUSPECTED/CONFIRMED
// is only used to reopen an incident within the cooldown period (deduplication).
var transitions = map[S][]S{
	model.IncidentSuspected:          {model.IncidentConfirmed, model.IncidentAcknowledged, model.IncidentResolved, model.IncidentUnknown},
	model.IncidentConfirmed:          {model.IncidentAcknowledged, model.IncidentRecoveryPending, model.IncidentRecovering, model.IncidentManualIntervention, model.IncidentResolved, model.IncidentUnknown},
	model.IncidentAcknowledged:       {model.IncidentConfirmed, model.IncidentRecoveryPending, model.IncidentRecovering, model.IncidentManualIntervention, model.IncidentResolved, model.IncidentUnknown},
	model.IncidentRecoveryPending:    {model.IncidentRecovering, model.IncidentManualIntervention, model.IncidentAcknowledged, model.IncidentResolved},
	model.IncidentRecovering:         {model.IncidentFailedOver, model.IncidentManualIntervention, model.IncidentResolved},
	model.IncidentFailedOver:         {model.IncidentManualIntervention, model.IncidentResolved},
	model.IncidentManualIntervention: {model.IncidentAcknowledged, model.IncidentRecovering, model.IncidentResolved},
	model.IncidentUnknown:            {model.IncidentSuspected, model.IncidentConfirmed, model.IncidentAcknowledged, model.IncidentResolved},
	model.IncidentResolved:           {model.IncidentSuspected, model.IncidentConfirmed},
}

// CanTransition reports whether from → to is permitted.
func CanTransition(from, to S) bool {
	for _, t := range transitions[from] {
		if t == to {
			return true
		}
	}
	return false
}

// ErrInvalidTransition describes a rejected transition.
type ErrInvalidTransition struct{ From, To S }

func (e ErrInvalidTransition) Error() string {
	return fmt.Sprintf("invalid incident transition %s → %s", e.From, e.To)
}

// IsOpen reports whether an incident status is unresolved.
func IsOpen(s S) bool { return s != model.IncidentResolved }

// IsConfirmedOrLater reports whether the incident passed confirmation.
func IsConfirmedOrLater(s S) bool {
	return s != model.IncidentSuspected && s != model.IncidentUnknown
}
