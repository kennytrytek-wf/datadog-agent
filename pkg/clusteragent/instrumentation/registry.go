// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016-present Datadog, Inc.

//go:build kubeapiserver

package instrumentation

import datadoghq "github.com/DataDog/datadog-operator/api/datadoghq/v1alpha1"

// Registry owns the ordered set of DatadogInstrumentation product handlers.
type Registry struct {
	handlers []Handler
}

// NewRegistry creates a deterministic handler registry. Handler order is preserved.
func NewRegistry(handlers ...Handler) *Registry {
	ordered := make([]Handler, 0, len(handlers))
	ordered = append(ordered, handlers...)
	return &Registry{handlers: ordered}
}

// Handlers returns all registered handlers in deterministic order.
func (r *Registry) Handlers() []Handler {
	if r == nil {
		return nil
	}
	handlers := make([]Handler, 0, len(r.handlers))
	handlers = append(handlers, r.handlers...)
	return handlers
}

// RelevantHandlers returns handlers whose section is present in the CR.
func (r *Registry) RelevantHandlers(cr *datadoghq.DatadogInstrumentation) []Handler {
	if r == nil || cr == nil {
		return nil
	}
	relevant := make([]Handler, 0, len(r.handlers))
	for _, handler := range r.handlers {
		if handler.HasSection(cr) {
			relevant = append(relevant, handler)
		}
	}
	return relevant
}

// Validate runs handler-owned validation in deterministic order.
func (r *Registry) Validate(cr *datadoghq.DatadogInstrumentation) []ValidationError {
	if r == nil || cr == nil {
		return nil
	}
	var errs []ValidationError
	for _, handler := range r.RelevantHandlers(cr) {
		errs = append(errs, handler.Validate(cr)...)
	}
	return errs
}
