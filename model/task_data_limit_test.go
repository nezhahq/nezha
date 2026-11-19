package model

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestServiceTargetIsBoundedBeforePersistence(t *testing.T) {
	service := &Service{Type: TaskTypeTCPPing, Target: strings.Repeat("4", MaxServiceTargetBytes)}
	require.NoError(t, service.BeforeSave(nil))

	service.Target += "4"
	require.Error(t, ValidateServiceTarget(service.Target))
	require.Error(t, service.BeforeSave(nil))
}

func TestCronCommandIsBoundedBeforePersistence(t *testing.T) {
	cron := &Cron{Command: strings.Repeat("#", MaxCronCommandBytes)}
	require.NoError(t, cron.BeforeSave(nil))

	cron.Command += "#"
	require.Error(t, ValidateCronCommand(cron.Command))
	require.Error(t, cron.BeforeSave(nil))
}
