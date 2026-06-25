package email

import (
	"context"
	"log/slog"
	"os"
	"strings"
)

// EnvMode is the boot-time mode-pick env var. Values:
//
//   - ""        (default) — smtp.
//   - "smtp"    — production SMTPSender.
//   - "capture" — in-memory recorder; outbound mail does NOT
//     leave the process. Emits a WARN at boot when picked
//     outside of a Go test process to surface the mode-flip on
//     dev / staging machines.
//   - "disabled" — DisabledSender; logs every send + drops it.
//     Safer than capture for "this stack should never email" envs.
const EnvMode = "AA_EMAIL_MODE"

// Mode is the resolved selection. Use [PickMode] to derive from
// env + the SMTP-configured fallback rule (no env + no SMTP host
// → disabled).
type Mode string

const (
	ModeSMTP     Mode = "smtp"
	ModeCapture  Mode = "capture"
	ModeDisabled Mode = "disabled"
)

// PickMode resolves the boot mode from the env var. Empty env =
// "smtp" (the safe production default). Unknown values fall back
// to "smtp" with a WARN — better to attempt configured SMTP than
// silently drop mail.
func PickMode(logger *slog.Logger) Mode {
	v := strings.ToLower(strings.TrimSpace(os.Getenv(EnvMode)))
	switch v {
	case "", "smtp":
		return ModeSMTP
	case "capture":
		warnIfNotTest(logger,
			"email.capture_mode_active",
			"AA_EMAIL_MODE=capture — outbound email is recorded in-memory + NEVER delivered. "+
				"This is for dev/staging only; ensure your production env DOES NOT set this.",
		)
		return ModeCapture
	case "disabled":
		return ModeDisabled
	default:
		if logger != nil {
			logger.LogAttrs(context.Background(), slog.LevelWarn,
				"email.unknown_mode",
				slog.String("value", v),
				slog.String("fallback", string(ModeSMTP)),
			)
		}
		return ModeSMTP
	}
}

// BuildSender wires a [Sender] for the chosen mode. The
// SMTPSender path takes a config provider so admin-side SMTP
// edits take effect without a restart.
func BuildSender(mode Mode, provider ConfigProvider, logger *slog.Logger) Sender {
	switch mode {
	case ModeCapture:
		return &Capture{}
	case ModeDisabled:
		return DisabledSender{Logger: slogWarner{logger: logger}}
	case ModeSMTP:
		fallthrough
	default:
		return NewSMTPSender(provider)
	}
}

// slogWarner adapts *slog.Logger to the small Warn-only interface
// DisabledSender expects (kept narrow so tests can pass a stub).
type slogWarner struct{ logger *slog.Logger }

func (w slogWarner) Warn(msg string, args ...any) {
	if w.logger == nil {
		return
	}
	w.logger.Warn(msg, args...)
}

// warnIfNotTest emits a WARN unless we're running under `go test`
// (detected via the standard `os.Args[0]` heuristic — Go test
// binaries always end in `.test`). The brief said capture mode
// must warn outside test environments; the heuristic keeps test
// runs quiet.
func warnIfNotTest(logger *slog.Logger, event, msg string) {
	if isTestProcess() {
		return
	}
	if logger == nil {
		return
	}
	logger.LogAttrs(context.Background(), slog.LevelWarn, event,
		slog.String("message", msg),
	)
}

func isTestProcess() bool {
	if len(os.Args) == 0 {
		return false
	}
	return strings.HasSuffix(os.Args[0], ".test") ||
		strings.Contains(os.Args[0], "/_test/") ||
		strings.HasSuffix(os.Args[0], "_test")
}
