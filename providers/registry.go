package providers

import "fmt"

// NewProvider returns a Provider for the given API convention ("anthropic" or "openai").
// baseURL is forwarded to the provider constructor unchanged.
func NewProvider(convention, baseURL string) (Provider, error) {
	switch convention {
	case "anthropic":
		return NewAnthropicProvider(baseURL), nil
	case "openai":
		return NewOpenAIProvider(baseURL), nil
	default:
		return nil, fmt.Errorf("providers: unknown convention %q (supported: anthropic, openai)", convention)
	}
}
