package incidents

import (
	"testing"

	"github.com/intrabin/intrabin/internal/model"
)

func TestTransitions(t *testing.T) {
	ok := [][2]model.IncidentStatus{
		{model.IncidentSuspected, model.IncidentConfirmed},
		{model.IncidentSuspected, model.IncidentResolved},
		{model.IncidentConfirmed, model.IncidentAcknowledged},
		{model.IncidentConfirmed, model.IncidentRecoveryPending},
		{model.IncidentRecoveryPending, model.IncidentRecovering},
		{model.IncidentRecovering, model.IncidentFailedOver},
		{model.IncidentRecovering, model.IncidentManualIntervention},
		{model.IncidentResolved, model.IncidentSuspected}, // reopen within cooldown
	}
	for _, c := range ok {
		if !CanTransition(c[0], c[1]) {
			t.Errorf("%s → %s should be allowed", c[0], c[1])
		}
	}
	bad := [][2]model.IncidentStatus{
		{model.IncidentResolved, model.IncidentAcknowledged},
		{model.IncidentSuspected, model.IncidentFailedOver},
		{model.IncidentFailedOver, model.IncidentConfirmed},
		{model.IncidentResolved, model.IncidentRecovering},
	}
	for _, c := range bad {
		if CanTransition(c[0], c[1]) {
			t.Errorf("%s → %s should be rejected", c[0], c[1])
		}
	}
}
