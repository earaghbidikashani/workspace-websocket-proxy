/*
Copyright (c) Amazon Web Services
Distributed under the terms of the MIT license
*/

package proxy

import "context"

// Revalidator periodically re-validates a session. Scaffolding, not yet implemented.
type Revalidator interface {
	Revalidate(ctx context.Context) error
}

// NoOpRevalidator is the default implementation that always returns valid.
type NoOpRevalidator struct{}

// Revalidate always returns nil (session always valid).
func (n *NoOpRevalidator) Revalidate(_ context.Context) error {
	return nil
}
