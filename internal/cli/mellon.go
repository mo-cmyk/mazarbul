package cli

// mellon.go exposes the device-pairing CLI surface. Pairing is pure
// local crypto (Argon2id + XChaCha20-Poly1305 via watchword.Seal), so
// a running daemon is not required and the commands work even when
// the four-storage stack is offline.

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/mo-cmyk/mazarbul/internal/config"
	"github.com/mo-cmyk/mazarbul/internal/identity"
	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/mellon"
	"github.com/mo-cmyk/mazarbul/internal/secret"
	"github.com/mo-cmyk/mazarbul/internal/watchword"
)

// newMellonCmd is the parent cobra command for `maz mellon`. Registered
// in root.go alongside the other top-level verbs.
func newMellonCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "mellon",
		Short: "Pair a new device into the Mazarbul sync mesh",
		Long: `Mellon ("friend") is the air-gapped pairing gate.

The existing device runs 'maz mellon code' to print a six-digit code
and a one-shot sealed bundle. The new device runs 'maz mellon pair',
types the code at the prompt, and pastes the bundle. On a successful
decrypt the bundle's Watchword, remote URLs, and device ID are
printed for inspection.

The code and bundle MUST travel through two different channels (one
read aloud, one pasted; one over Signal, one over AirDrop) — a
single-channel transcript exposes both to anyone who intercepts it.`,
	}
	cmd.AddCommand(newMellonCodeCmd())
	cmd.AddCommand(newMellonPairCmd())
	return cmd
}

func newMellonCodeCmd() *cobra.Command {
	var (
		watchword  string
		deviceID   string
		remotePers string
		remoteTeam string
		remoteProj string
		note       string
	)
	cmd := &cobra.Command{
		Use:   "code",
		Short: "Generate a one-shot pairing code + sealed bundle",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runMellonCode(cmd.Context(), cmd.OutOrStdout(),
				watchword, deviceID, remotePers, remoteTeam, remoteProj, note)
		},
	}
	cmd.Flags().StringVar(&watchword, "watchword", "", "BIP-39 mnemonic to embed (required)")
	cmd.Flags().StringVar(&deviceID, "device-id", "", "default device-id stamp the new device should use (required)")
	cmd.Flags().StringVar(&remotePers, "personal-remote", "", "git URL for the personal scope")
	cmd.Flags().StringVar(&remoteTeam, "team-remote", "", "git URL for the team scope")
	cmd.Flags().StringVar(&remoteProj, "project-remote", "", "git URL for the project scope")
	cmd.Flags().StringVar(&note, "note", "", "operator-facing note (free text)")
	return cmd
}

func runMellonCode(_ context.Context, out io.Writer,
	watchwordText, deviceID, pers, team, proj, note string,
) error {
	if strings.TrimSpace(watchwordText) == "" {
		return errors.New("--watchword is required")
	}
	if strings.TrimSpace(deviceID) == "" {
		return errors.New("--device-id is required")
	}
	bundle := mellon.PairBundle{
		Watchword:  strings.TrimSpace(watchwordText),
		DeviceID:   strings.TrimSpace(deviceID),
		Note:       note,
		RemoteURLs: map[kernel.Scope]string{},
	}
	if pers != "" {
		bundle.RemoteURLs[kernel.ScopePersonal] = pers
	}
	if team != "" {
		bundle.RemoteURLs[kernel.ScopeTeam] = team
	}
	if proj != "" {
		bundle.RemoteURLs[kernel.ScopeProject] = proj
	}
	code, err := mellon.GenerateCode()
	if err != nil {
		return fmt.Errorf("mellon code: %w", err)
	}
	wire, err := mellon.Generate(code, bundle)
	if err != nil {
		return fmt.Errorf("mellon code: %w", err)
	}
	w := &writeBuffer{w: out}
	w.line("Mellon — speak, friend, and enter")
	w.line("")
	w.line("  Code:   " + code)
	w.line("  Bundle: " + wire)
	w.line("")
	w.line("Send the code and bundle to the new device through TWO different")
	w.line("channels (one read aloud, one pasted). The code is one-shot —")
	w.line("treat each generated pair as single-use.")
	return w.err
}

func newMellonPairCmd() *cobra.Command {
	var (
		codeFlag   string
		bundleFlag string
		printOnly  bool
	)
	cmd := &cobra.Command{
		Use:   "pair",
		Short: "Decrypt a pairing bundle from another device",
		RunE: func(cmd *cobra.Command, _ []string) error {
			dataDir, _ := cmd.Flags().GetString("data-dir")
			return runMellonPair(cmd.Context(), cmd.InOrStdin(), cmd.OutOrStdout(), dataDir, codeFlag, bundleFlag, printOnly)
		},
	}
	cmd.Flags().StringVar(&codeFlag, "code", "", "6-digit pairing code (prompted if omitted)")
	cmd.Flags().StringVar(&bundleFlag, "bundle", "", "base64 sealed bundle (prompted if omitted)")
	cmd.Flags().BoolVar(&printOnly, "print-only", false, "decrypt + display only; do NOT persist Watchword or remotes")
	return cmd
}

func runMellonPair(ctx context.Context, in io.Reader, out io.Writer, dataDir, codeFlag, bundleFlag string, printOnly bool) error {
	w := &writeBuffer{w: out}
	scanner := bufio.NewScanner(in)
	scanner.Buffer(make([]byte, 0, 4096), 1<<20)
	code := codeFlag
	if code == "" {
		code = promptLine(scanner, w, "Pair code (6 digits): ")
	}
	wire := bundleFlag
	if wire == "" {
		wire = promptLine(scanner, w, "Bundle (one line, base64): ")
	}
	bundle, err := mellon.Pair(strings.TrimSpace(code), strings.TrimSpace(wire))
	if err != nil {
		return fmt.Errorf("mellon pair: %w", err)
	}
	w.line("Pairing succeeded.")
	w.line("  Device ID:  " + bundle.DeviceID)
	w.line("  Note:       " + bundle.Note)
	w.line("  Remotes:")
	for scope, url := range bundle.RemoteURLs {
		w.line(fmt.Sprintf("    %-9s %s", string(scope)+":", url))
	}
	if printOnly {
		w.line("")
		w.line("  Watchword (24 words — copy to a secure store, then continue):")
		w.line("    " + bundle.Watchword)
		w.line("")
		w.line("--print-only set: NOT persisted. To apply on this device:")
		w.line("  maz init --restore --watchword \"<the 24 words above>\"")
		return w.err
	}
	if applyErr := applyPairBundle(ctx, dataDir, bundle); applyErr != nil {
		return fmt.Errorf("mellon pair: persist: %w", applyErr)
	}
	w.line("")
	w.line("Persisted: Watchword stored in OS keychain, remotes written to state.toml.")
	w.line("Run `maz doctor` to verify the new device is sync-ready, then `maz sync pull` to fetch.")
	return w.err
}

// applyPairBundle saves the paired Watchword to the keychain, provisions the
// per-device signing identity (bundle device id + a random device subkey),
// and writes the per-scope remote URLs into state.toml. Designed to be
// idempotent: running pair twice with the same bundle (Watchword unchanged,
// remotes unchanged) is a no-op past the first call.
func applyPairBundle(ctx context.Context, dataDir string, bundle mellon.PairBundle) error {
	if dataDir == "" {
		return errors.New("data-dir is required when persisting (omit --print-only or pass --data-dir)")
	}
	chain := newSecretChain()
	if err := chain.Set(ctx, watchword.KeychainKey, bundle.Watchword); err != nil {
		return fmt.Errorf("store watchword: %w", err)
	}
	state, lerr := config.LoadUserState(dataDir)
	if lerr != nil && !errors.Is(lerr, config.ErrUserStateNotFound) {
		return fmt.Errorf("load state.toml: %w", lerr)
	}
	if err := provisionPairedDevice(ctx, chain, state.Identity.PrincipalUID, bundle.DeviceID); err != nil {
		return err
	}
	if state.Sync.Remotes == nil {
		state.Sync.Remotes = map[string]config.ScopeRemoteStateTable{}
	}
	for scope, url := range bundle.RemoteURLs {
		entry := state.Sync.Remotes[string(scope)]
		entry.URL = url
		entry.AuthMethod = inferMellonAuthMethod(url)
		state.Sync.Remotes[string(scope)] = entry
	}
	if err := config.WriteUserState(dataDir, state); err != nil {
		return fmt.Errorf("write state.toml: %w", err)
	}
	return nil
}

// provisionPairedDevice gives a freshly paired device its own signing
// identity: it adopts the device id the sender stamped into the bundle (an
// already-identified device keeps its id) and mints a random per-device
// ed25519 subkey. The subkey is minted ONLY for a device that has never
// completed identity provisioning (no self→principal binding): an
// established device — device 0 in particular — must never silently switch
// to an unenrolled key. The subkey's principal-vouched deviceEnrolled event
// is appended by the daemon's identity seeding on the next boot, once the
// principal is recovered from the synced log.
func provisionPairedDevice(ctx context.Context, chain secret.Provider, boundPrincipalUID, bundleDeviceID string) error {
	idStore := identity.New(chain)
	if bundleDeviceID != "" {
		if _, err := idStore.AdoptDeviceID(ctx, bundleDeviceID); err != nil {
			return fmt.Errorf("adopt device id: %w", err)
		}
	}
	if boundPrincipalUID != "" {
		return nil // established device: keep its active signing key
	}
	hasSub, err := idStore.HasDeviceSubkey(ctx)
	if err != nil {
		return fmt.Errorf("check device subkey: %w", err)
	}
	if hasSub {
		return nil
	}
	if _, err := idStore.MintDeviceSubkey(ctx); err != nil {
		return fmt.Errorf("mint device subkey: %w", err)
	}
	return nil
}

// inferMellonAuthMethod is a tiny mirror of the wizard's
// inferAuthMethod; kept local so this file does not depend on the TUI
// wizard package. SSH/HTTPS PAT/file are the documented set.
const (
	authMethodSSH   = "ssh"
	authMethodHTTPS = "https_pat"
	authMethodNone  = "none"
)

func inferMellonAuthMethod(url string) string {
	switch {
	case strings.HasPrefix(url, "git@"), strings.HasPrefix(url, "ssh://"):
		return authMethodSSH
	case strings.HasPrefix(url, "https://"), strings.HasPrefix(url, "http://"):
		return authMethodHTTPS
	case strings.HasPrefix(url, "file://"):
		return authMethodNone
	default:
		return authMethodSSH
	}
}

// writeBuffer is a tiny io.Writer wrapper that latches the first
// error and silently no-ops subsequent writes. Keeps the lint surface
// clean (no errcheck litter on every Fprintln) without obscuring the
// genuine "stdout closed" case the caller might want to surface.
type writeBuffer struct {
	w   io.Writer
	err error
}

func (b *writeBuffer) line(s string) {
	if b.err != nil {
		return
	}
	if _, err := fmt.Fprintln(b.w, s); err != nil {
		b.err = fmt.Errorf("mellon: write output: %w", err)
	}
}

// promptLine prints prompt and reads one line from the shared
// scanner. Takes a *bufio.Scanner instead of an io.Reader so multiple
// prompts in one call drain a single buffered stream — a fresh
// bufio.NewScanner per prompt reads ahead, consuming a later prompt's
// input on the first read. EOF mid-prompt yields the empty string;
// downstream validation rejects empties.
func promptLine(scanner *bufio.Scanner, b *writeBuffer, prompt string) string {
	if b.err != nil {
		return ""
	}
	if _, err := fmt.Fprint(b.w, prompt); err != nil {
		b.err = fmt.Errorf("mellon: write prompt: %w", err)
		return ""
	}
	if !scanner.Scan() {
		return ""
	}
	return scanner.Text()
}
