package tickets

import (
	"context"
	"net"
	"strings"

	"transithub/backend/internal/security/egress"
)

type srcHostValidator func(context.Context, string) (string, error)

func normalizeSrcHost(ctx context.Context, value string) (string, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return "", requestError(ErrorEmbedInvalidSrcHost)
	}
	if !strings.Contains(trimmed, "://") {
		trimmed = "https://" + trimmed
	}
	parsed, err := egress.ValidatePublicHTTPS(ctx, trimmed, net.DefaultResolver)
	if err != nil {
		return "", requestError(ErrorEmbedInvalidSrcHost)
	}
	parsed.Path = ""
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return strings.TrimRight(parsed.String(), "/"), nil
}
