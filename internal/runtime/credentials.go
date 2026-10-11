package runtime

import "context"

// Credentials supplies job-bound credentials and log redaction for an embedder.
type Credentials interface {
	ResolveSecret(context.Context, string) (string, error)
	AddRedaction(context.Context, string) error
	GitCredentialHelper() (string, error)
}

type credentialsKey struct{}

// WithCredentials selects invocation-local credential services.
func WithCredentials(ctx context.Context, credentials Credentials) context.Context {
	return context.WithValue(ctx, credentialsKey{}, credentials)
}

// InvocationCredentials returns the explicitly supplied services, if any.
func InvocationCredentials(ctx context.Context) Credentials {
	credentials, _ := ctx.Value(credentialsKey{}).(Credentials)
	return credentials
}
