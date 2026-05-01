// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016-present Datadog, Inc.

//go:build kubeapiserver && test

package instrumentation

import (
	"errors"
	"testing"

	datadoghq "github.com/DataDog/datadog-operator/api/datadoghq/v1alpha1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	dynamicfake "k8s.io/client-go/dynamic/fake"
)

func TestReconcile(t *testing.T) {
	checksPresent := func(cr *datadoghq.DatadogInstrumentation) bool {
		return cr != nil && len(cr.Spec.Config.Checks) > 0
	}

	crWith := newTestCR("test-ddi", "default", 1, defaultChecks())
	crWithout := newTestCR("test-ddi", "default", 1, nil)

	tests := []struct {
		name          string
		snapshot      eventSnapshot
		wantEventType EventType
		wantCallCount int
		wantCR        *datadoghq.DatadogInstrumentation
		handlerErr    error
		wantReconcErr bool
	}{
		{
			name:          "add event dispatches EventCreate",
			snapshot:      eventSnapshot{old: nil, new: crWith},
			wantEventType: EventCreate,
			wantCallCount: 1,
			wantCR:        crWith,
		},
		{
			name:          "update event dispatches EventUpdate",
			snapshot:      eventSnapshot{old: crWith, new: crWith},
			wantEventType: EventUpdate,
			wantCallCount: 1,
			wantCR:        crWith,
		},
		{
			name:          "delete event (new nil) dispatches EventDelete with old CR",
			snapshot:      eventSnapshot{old: crWith, new: nil},
			wantEventType: EventDelete,
			wantCallCount: 1,
			wantCR:        crWith,
		},
		{
			name:          "delete event (section removed) dispatches EventDelete",
			snapshot:      eventSnapshot{old: crWith, new: crWithout},
			wantEventType: EventDelete,
			wantCallCount: 1,
			wantCR:        crWith,
		},
		{
			name:          "no section in either old or new results in no handler call",
			snapshot:      eventSnapshot{old: crWithout, new: crWithout},
			wantCallCount: 0,
		},
		{
			name:          "handler error propagates",
			snapshot:      eventSnapshot{old: nil, new: crWith},
			wantEventType: EventCreate,
			wantCallCount: 1,
			wantCR:        crWith,
			handlerErr:    errors.New("handler failed"),
			wantReconcErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handler := &mockHandler{
				hasSectionFunc: checksPresent,
				conditionType:  "ChecksReady",
				handleErr:      tt.handlerErr,
			}

			c := &Controller{
				handlers: []Handler{handler},
				isLeader: func() bool { return false },
			}

			err := c.reconcile(t.Context(), tt.snapshot)
			if tt.wantReconcErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}

			calls := handler.getCalls()
			assert.Len(t, calls, tt.wantCallCount)
			if tt.wantCallCount > 0 {
				assert.Equal(t, tt.wantEventType, calls[0].eventType)
				assert.Equal(t, tt.wantCR.Name, calls[0].cr.Name)
			}
		})
	}
}

func TestReconcileMultipleHandlers(t *testing.T) {
	crWith := newTestCR("test-ddi", "default", 1, defaultChecks())

	handlerA := &mockHandler{
		name:           "handler-a",
		hasSectionFunc: func(cr *datadoghq.DatadogInstrumentation) bool { return len(cr.Spec.Config.Checks) > 0 },
		conditionType:  "AReady",
	}
	handlerB := &mockHandler{
		name:           "handler-b",
		hasSectionFunc: func(_ *datadoghq.DatadogInstrumentation) bool { return false },
		conditionType:  "BReady",
	}

	c := &Controller{
		handlers: []Handler{handlerA, handlerB},
		isLeader: func() bool { return false },
	}

	err := c.reconcile(t.Context(), eventSnapshot{old: nil, new: crWith})
	require.NoError(t, err)

	assert.Len(t, handlerA.getCalls(), 1, "handler-a should be called")
	assert.Equal(t, EventCreate, handlerA.getCalls()[0].eventType)

	assert.Empty(t, handlerB.getCalls(), "handler-b should not be called (no section)")
}

func TestReconcile_UpdateStatusOnlyAsLeader(t *testing.T) {
	tests := []struct {
		name             string
		isLeader         bool
		wantStatusUpdate bool
	}{
		{
			name:             "leader writes status condition",
			isLeader:         true,
			wantStatusUpdate: true,
		},
		{
			name:             "non-leader skips status update",
			isLeader:         false,
			wantStatusUpdate: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			crWith := newTestCR("test-ddi", "default", 1, defaultChecks())

			handler := &mockHandler{
				hasSectionFunc: func(cr *datadoghq.DatadogInstrumentation) bool { return len(cr.Spec.Config.Checks) > 0 },
				handleStatus: HandlerStatus{
					Type:    "ChecksReady",
					Status:  metav1.ConditionTrue,
					Reason:  "Configured",
					Message: "all good",
				},
			}

			scheme := fakeScheme()
			fakeDynClient := dynamicfake.NewSimpleDynamicClient(scheme, crWith)

			c := &Controller{
				statusClient: fakeDynClient,
				handlers:     []Handler{handler},
				isLeader:     func() bool { return tt.isLeader },
			}

			err := c.reconcile(t.Context(), eventSnapshot{old: nil, new: crWith})
			require.NoError(t, err)

			require.Len(t, handler.getCalls(), 1, "handler should always be called regardless of leadership")

			hasStatusUpdate := false
			for _, action := range fakeDynClient.Actions() {
				if action.GetVerb() == "update" && action.GetSubresource() == "status" {
					hasStatusUpdate = true
					break
				}
			}

			if tt.wantStatusUpdate {
				assert.True(t, hasStatusUpdate, "leader should write status condition")
			} else {
				assert.False(t, hasStatusUpdate, "non-leader should not write status condition")
			}
		})
	}
}

func TestReconcileDeleteSkipsStatusUpdate(t *testing.T) {
	crWith := newTestCR("test-ddi", "default", 1, defaultChecks())

	handler := &mockHandler{
		hasSectionFunc: func(cr *datadoghq.DatadogInstrumentation) bool { return len(cr.Spec.Config.Checks) > 0 },
		handleStatus: HandlerStatus{
			Type:    "ChecksReady",
			Status:  metav1.ConditionTrue,
			Reason:  "Configured",
			Message: "all good",
		},
	}

	scheme := fakeScheme()
	fakeDynClient := dynamicfake.NewSimpleDynamicClient(scheme, crWith)

	c := &Controller{
		statusClient: fakeDynClient,
		handlers:     []Handler{handler},
		isLeader:     func() bool { return true },
	}

	err := c.reconcile(t.Context(), eventSnapshot{old: crWith, new: nil})
	require.NoError(t, err)

	calls := handler.getCalls()
	require.Len(t, calls, 1)
	assert.Equal(t, EventDelete, calls[0].eventType)

	for _, action := range fakeDynClient.Actions() {
		assert.NotEqual(t, "update", action.GetVerb(), "should not attempt status update on delete")
	}
}
