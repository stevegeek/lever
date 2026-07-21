package cli

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"time"

	"github.com/spf13/cobra"
	"github.com/stevegeek/lever/internal/brokerctl"
	"github.com/stevegeek/lever/internal/config"
	"github.com/stevegeek/lever/internal/opsig"
)

// newDirectiveCmd is the operator's authenticated channel to a running agent:
// send/list/revoke signed directives, plus a selftest that probes the
// signing key against the broker's allowed_signers. Every subcommand talks
// to the broker's 0600 UNIX-socket admin channel (brokerctl.State.DirectiveSock),
// never the loopback admin port — filesystem permissions are the gate.
func newDirectiveCmd() *cobra.Command {
	c := &cobra.Command{Use: "directive", Short: "Send authenticated operator directives to an agent"}
	c.AddCommand(newDirectiveSendCmd(), newDirectiveListCmd(), newDirectiveRevokeCmd(), newDirectiveSelftestCmd())
	return c
}

// udsClient builds an http.Client that dials sock (a UNIX socket) for every
// request regardless of the request URL's host — callers use the placeholder
// authority "http://lever/..." since only the Transport's DialContext matters.
func udsClient(sock string) *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", sock)
			},
		},
	}
}

// loadAppAndState resolves the config path (an optional trailing CONFIG arg)
// and loads both the App and its beside-the-config state dir, mirroring
// newBrokerServeCmd's path+app+stateFor sequence (broker.go) since directive
// commands need all three: app for Name/Operator/expiry accessors, state for
// DirectiveSock.
func loadAppAndState(args []string) (*config.App, brokerctl.State, error) {
	path, err := resolveConfigPath(argOrEmpty(args))
	if err != nil {
		return nil, brokerctl.State{}, err
	}
	app, err := config.Load(path)
	if err != nil {
		return nil, brokerctl.State{}, err
	}
	return app, stateFor(path), nil
}

// signingKey resolves the operator private key: the --key flag if given,
// else app.Operator.SigningKey, else an error (a directive can never be
// signed with an implicit/missing key).
func signingKey(flagKey string, app *config.App) (string, error) {
	if flagKey != "" {
		return flagKey, nil
	}
	if app.Operator.SigningKey != "" {
		return app.Operator.SigningKey, nil
	}
	return "", fmt.Errorf("directive: no signing key — pass --key or set operator.signing_key in %s", config.CanonicalName)
}

// newDirectiveID mints a directive id: 16 crypto/rand bytes formatted as a
// UUID (8-4-4-4-12 hex). Directive ids are a replay-defence key (see
// DirectiveStore.Submit), not just a display label, so they must be
// unpredictable — crypto/rand, not math/rand.
func newDirectiveID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("directive: id: %w", err)
	}
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}

// udsDo issues a request against sock+path (method/body as given), returning
// the decoded JSON response body into out (skipped if out is nil). A non-2xx
// status is an error carrying the response body for diagnosis.
func udsDo(ctx context.Context, sock, method, path string, body []byte, out any) error {
	cl := udsClient(sock)
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://lever"+path, rdr)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := cl.Do(req)
	if err != nil {
		return fmt.Errorf("directive: contacting broker's directive channel (is `lever broker serve` running with operator directives enabled?): %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("directive: %s %s: %d: %s", method, path, resp.StatusCode, bytes.TrimSpace(respBody))
	}
	if out != nil {
		if err := json.Unmarshal(respBody, out); err != nil {
			return fmt.Errorf("directive: %s %s: bad response: %w", method, path, err)
		}
	}
	return nil
}

type directiveResolveResp struct {
	CN         string `json:"cn"`
	Slug       string `json:"slug"`
	Generation int    `json:"generation"`
}

// signAndPostStatement marshals st, prints it for operator review (the
// operator must be able to see exactly what they're about to sign), signs
// it under opsig.NamespaceDirective, and POSTs the {statement,signature}
// envelope to path.
func signAndPostStatement(cmd *cobra.Command, sock, keyPath, path string, st opsig.Statement, out any) error {
	raw, err := json.Marshal(st)
	if err != nil {
		return err
	}
	cmd.Println("signing exactly these bytes:")
	cmd.Println(string(raw))
	sig, err := opsig.Sign(keyPath, opsig.NamespaceDirective, raw)
	if err != nil {
		return fmt.Errorf("directive: sign: %w", err)
	}
	reqBody, err := json.Marshal(map[string]string{
		"statement": base64.StdEncoding.EncodeToString(raw),
		"signature": base64.StdEncoding.EncodeToString(sig),
	})
	if err != nil {
		return err
	}
	return udsDo(cmd.Context(), sock, http.MethodPost, path, reqBody, out)
}

// signAndPostEnvelope builds, signs (opsig.NamespaceAdmin), and POSTs an
// admin-op envelope (list/revoke).
func signAndPostEnvelope(cmd *cobra.Command, sock, keyPath, appName, op string, params map[string]string, path string, out any) error {
	env := opsig.Envelope{V: 1, Instance: appName, Op: op, Params: params, IssuedAt: time.Now().Format(time.RFC3339)}
	raw, err := json.Marshal(env)
	if err != nil {
		return err
	}
	sig, err := opsig.Sign(keyPath, opsig.NamespaceAdmin, raw)
	if err != nil {
		return fmt.Errorf("directive: sign: %w", err)
	}
	reqBody, err := json.Marshal(map[string]string{
		"envelope":  base64.StdEncoding.EncodeToString(raw),
		"signature": base64.StdEncoding.EncodeToString(sig),
	})
	if err != nil {
		return err
	}
	return udsDo(cmd.Context(), sock, http.MethodPost, path, reqBody, out)
}

func newDirectiveSendCmd() *cobra.Command {
	var instruction, actionJSON, key, expiresFlag, notBeforeFlag string
	c := &cobra.Command{
		Use:   "send <agent> (--instruction TEXT | --action JSON) [CONFIG]",
		Args:  cobra.RangeArgs(1, 2),
		Short: "Sign and send an operator directive to an agent",
		RunE: func(cmd *cobra.Command, args []string) error {
			agent := args[0]
			if (instruction == "") == (actionJSON == "") {
				return fmt.Errorf("directive send: exactly one of --instruction or --action is required")
			}

			var action opsig.Action
			if actionJSON != "" {
				if err := json.Unmarshal([]byte(actionJSON), &action); err != nil {
					return fmt.Errorf("directive send: --action: invalid JSON: %w", err)
				}
			} else {
				action = opsig.Action{Kind: "instruction", Text: instruction}
			}
			if err := opsig.ValidateAction(action); err != nil {
				return fmt.Errorf("directive send: invalid action: %w", err)
			}

			app, st, err := loadAppAndState(args[1:])
			if err != nil {
				return err
			}
			keyPath, err := signingKey(key, app)
			if err != nil {
				return err
			}

			var resolved directiveResolveResp
			if err := udsDo(cmd.Context(), st.DirectiveSock(), http.MethodGet,
				"/directive/resolve?agent="+url.QueryEscape(agent), nil, &resolved); err != nil {
				return err
			}

			expiry := app.EffectiveDirectiveExpiry()
			if expiresFlag != "" {
				d, err := time.ParseDuration(expiresFlag)
				if err != nil {
					return fmt.Errorf("directive send: --expires: %w", err)
				}
				expiry = d
			}
			if expiry > app.EffectiveDirectiveExpiryMax() {
				return fmt.Errorf("directive send: expiry %s exceeds this instance's cap %s", expiry, app.EffectiveDirectiveExpiryMax())
			}

			now := time.Now()
			notBefore := now
			if notBeforeFlag != "" {
				nb, err := time.Parse(time.RFC3339, notBeforeFlag)
				if err != nil {
					return fmt.Errorf("directive send: --not-before: %w", err)
				}
				notBefore = nb
			}

			id, err := newDirectiveID()
			if err != nil {
				return err
			}
			st2 := opsig.Statement{
				V:           1,
				Instance:    app.Name,
				DirectiveID: id,
				TargetAgent: opsig.Target{CN: resolved.CN, Generation: resolved.Generation},
				IssuedAt:    now.Format(time.RFC3339),
				NotBefore:   notBefore.Format(time.RFC3339),
				ExpiresAt:   now.Add(expiry).Format(time.RFC3339),
				Action:      action,
			}

			var out map[string]any
			if err := signAndPostStatement(cmd, st.DirectiveSock(), keyPath, "/directive/send", st2, &out); err != nil {
				return err
			}
			cmd.Printf("directive %v sent: delivered=%v\n", out["id"], out["delivered"])
			return nil
		},
	}
	c.Flags().StringVar(&instruction, "instruction", "", "advisory instruction text (mutually exclusive with --action)")
	c.Flags().StringVar(&actionJSON, "action", "", "raw JSON action object (mutually exclusive with --instruction)")
	c.Flags().StringVar(&key, "key", "", "operator signing key path (default: operator.signing_key from config)")
	c.Flags().StringVar(&expiresFlag, "expires", "", "directive lifetime, e.g. 10m (default: operator.directive_expiry from config)")
	c.Flags().StringVar(&notBeforeFlag, "not-before", "", "RFC3339 time before which the directive is not valid (default: now)")
	return c
}

func newDirectiveListCmd() *cobra.Command {
	var key, stateFilter string
	c := &cobra.Command{
		Use:   "list [CONFIG]",
		Args:  cobra.MaximumNArgs(1),
		Short: "List operator directives known to the broker",
		RunE: func(cmd *cobra.Command, args []string) error {
			app, st, err := loadAppAndState(args)
			if err != nil {
				return err
			}
			keyPath, err := signingKey(key, app)
			if err != nil {
				return err
			}
			var out struct {
				Directives []json.RawMessage `json:"directives"`
			}
			if err := signAndPostEnvelope(cmd, st.DirectiveSock(), keyPath, app.Name, "list", nil, "/directive/list", &out); err != nil {
				return err
			}
			printed := 0
			for _, raw := range out.Directives {
				if stateFilter != "" {
					var probe struct {
						State string `json:"state"`
					}
					if err := json.Unmarshal(raw, &probe); err == nil && probe.State != stateFilter {
						continue
					}
				}
				cmd.Println(string(raw))
				printed++
			}
			if printed == 0 {
				cmd.Println("(no directives)")
			}
			return nil
		},
	}
	c.Flags().StringVar(&key, "key", "", "operator signing key path (default: operator.signing_key from config)")
	c.Flags().StringVar(&stateFilter, "state", "", "filter by state (active|consumed|revoked|invalidated|expired)")
	return c
}

func newDirectiveRevokeCmd() *cobra.Command {
	var key string
	c := &cobra.Command{
		Use:   "revoke <id> [CONFIG]",
		Args:  cobra.RangeArgs(1, 2),
		Short: "Revoke a directive by id",
		RunE: func(cmd *cobra.Command, args []string) error {
			id := args[0]
			app, st, err := loadAppAndState(args[1:])
			if err != nil {
				return err
			}
			keyPath, err := signingKey(key, app)
			if err != nil {
				return err
			}
			var out map[string]any
			if err := signAndPostEnvelope(cmd, st.DirectiveSock(), keyPath, app.Name, "revoke",
				map[string]string{"id": id}, "/directive/revoke", &out); err != nil {
				return err
			}
			cmd.Printf("directive %s revoked=%v\n", id, out["revoked"])
			return nil
		},
	}
	c.Flags().StringVar(&key, "key", "", "operator signing key path (default: operator.signing_key from config)")
	return c
}

func newDirectiveSelftestCmd() *cobra.Command {
	var key string
	c := &cobra.Command{
		Use:   "selftest [CONFIG]",
		Args:  cobra.MaximumNArgs(1),
		Short: "Verify the operator signing key against the broker's allowed_signers",
		RunE: func(cmd *cobra.Command, args []string) error {
			app, st, err := loadAppAndState(args)
			if err != nil {
				return err
			}
			keyPath, err := signingKey(key, app)
			if err != nil {
				return err
			}
			now := time.Now()
			id, err := newDirectiveID()
			if err != nil {
				return err
			}
			stmt := opsig.Statement{
				V:           1,
				Instance:    app.Name,
				DirectiveID: id,
				TargetAgent: opsig.Target{CN: "selftest", Generation: 1},
				IssuedAt:    now.Format(time.RFC3339),
				NotBefore:   now.Format(time.RFC3339),
				ExpiresAt:   now.Add(time.Minute).Format(time.RFC3339),
				Action:      opsig.Action{Kind: "instruction", Text: "selftest"},
			}
			var out map[string]any
			if err := signAndPostStatement(cmd, st.DirectiveSock(), keyPath, "/directive/selftest", stmt, &out); err != nil {
				cmd.Printf("selftest FAILED: %v\n", err)
				return err
			}
			cmd.Println("selftest OK: signing key verifies against the broker's allowed_signers")
			return nil
		},
	}
	c.Flags().StringVar(&key, "key", "", "operator signing key path (default: operator.signing_key from config)")
	return c
}
