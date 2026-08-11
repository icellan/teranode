package util

import (
	"fmt"
	"net"
	"strings"

	"github.com/bsv-blockchain/teranode/errors"
	"github.com/labstack/echo/v4"
)

// TrustedProxyIPExtractor builds an echo.IPExtractor that trusts
// X-Forwarded-For only from the given pipe-separated CIDR ranges. When
// trustedProxyCIDRs is empty, it returns echo's default extractor (trusts
// loopback and RFC1918 ranges).
//
// When trustedProxyCIDRs is non-empty but no valid CIDRs are parsed, this
// fails loudly rather than silently falling back to "trust all private
// ranges" - operator typos must not weaken the trust boundary.
//
// settingLabel is used only to prefix the returned configuration error, e.g.
// "[Asset] asset_trustedProxyCIDRs" or "[P2P] p2p_trustedProxyCIDRs".
func TrustedProxyIPExtractor(trustedProxyCIDRs, settingLabel string) (echo.IPExtractor, error) {
	if trustedProxyCIDRs == "" {
		return echo.ExtractIPFromXFFHeader(), nil
	}

	var trustOpts []echo.TrustOption

	var parseErrors []string

	for _, cidrStr := range strings.Split(trustedProxyCIDRs, "|") {
		cidrStr = strings.TrimSpace(cidrStr)
		if cidrStr == "" {
			continue
		}

		_, ipNet, err := net.ParseCIDR(cidrStr)
		if err != nil {
			parseErrors = append(parseErrors, fmt.Sprintf("%q (%v)", cidrStr, err))
			continue
		}

		trustOpts = append(trustOpts, echo.TrustIPRange(ipNet))
	}

	if len(trustOpts) == 0 {
		return nil, errors.NewConfigurationError(
			"%s is set but no valid CIDRs were parsed: %s",
			settingLabel, strings.Join(parseErrors, ", "),
		)
	}

	if len(parseErrors) > 0 {
		// Some valid, some invalid: still fail. Mixed input is almost
		// always a typo and silently using only the valid subset masks it.
		return nil, errors.NewConfigurationError(
			"%s contains invalid entries: %s",
			settingLabel, strings.Join(parseErrors, ", "),
		)
	}

	return echo.ExtractIPFromXFFHeader(trustOpts...), nil
}
