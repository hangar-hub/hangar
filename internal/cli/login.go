package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/jamesagarside/hangar/internal/api/gen"
)

// deviceClientID names the CLI in device-code logins; the Token it creates
// is named after it.
const deviceClientID = "hangar-cli"

// openBrowser opens a URL in the user's browser. Tests replace it.
var openBrowser = func(u string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", u)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", u)
	default:
		cmd = exec.Command("xdg-open", u)
	}
	return cmd.Start()
}

func (a *app) loginCommand() *cobra.Command {
	var noBrowser bool
	cmd := &cobra.Command{
		Use:   "login",
		Short: "Sign in to an Instance and save the Token",
		Long: `Signs in to an Instance. Without --token, starts a device-code login: the
browser opens a page on the Instance where you approve the code shown here,
and the Instance creates a write Token for the CLI.

The URL and username are saved in ~/.config/hangar/config.yaml. The Token is
saved in the OS keychain where one is available, otherwise in that file with
mode 0600. Pass --token - to read the Token from standard input.`,
		Example: `  hangar login --url https://hangar.example.com
  hangar login --url http://localhost:8080 --token hgr_xxxxxxxx
  echo "$TOKEN" | hangar login --url https://hangar.example.com --token -`,
		Args: args(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.with(cmd).login(cmd.Context(), cmd.InOrStdin(), noBrowser)
		},
	}
	a.clientFlags(cmd)
	cmd.Flags().BoolVar(&noBrowser, "no-browser", false, "print the approval link instead of opening a browser")
	return cmd
}

func (a *app) login(ctx context.Context, stdin io.Reader, noBrowser bool) error {
	s, path, err := loadSettings()
	if err != nil {
		return err
	}
	base, err := a.instanceURL(s)
	if err != nil {
		return err
	}
	token := a.token
	if token == "-" {
		line, err := bufio.NewReader(stdin).ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return err
		}
		token = strings.TrimSpace(line)
		if token == "" {
			return usagef("no Token on standard input")
		}
	}
	if token == "" {
		if token, err = a.deviceLogin(ctx, base, noBrowser); err != nil {
			return err
		}
	}
	c, err := newClient(base, token, a.opts.Version, a.http)
	if err != nil {
		return err
	}
	me, err := c.me(ctx)
	if err != nil {
		return err
	}
	if s.URL != "" && s.URL != base.String() {
		s.forgetToken()
	}
	s.URL = base.String()
	s.Username = me.Username
	inKeychain := s.storeToken(token)
	if err := s.save(path); err != nil {
		return err
	}
	if a.json {
		return a.printJSON(map[string]any{"url": s.URL, "username": me.Username, "token_in_keychain": inKeychain})
	}
	where := "the OS keychain"
	if !inKeychain {
		where = path
	}
	a.printf("Signed in to %s as %s. The Token is saved in %s.\n", s.URL, me.Username, where)
	return nil
}

// deviceCode is the answer to POST /oauth/device (RFC 8628).
type deviceCode struct {
	DeviceCode              string `json:"device_code"`
	UserCode                string `json:"user_code"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete"`
	Interval                int    `json:"interval"`
	ExpiresIn               int    `json:"expires_in"`
}

// deviceLogin runs the device-code flow against the Instance's /oauth
// endpoints and returns the Token.
func (a *app) deviceLogin(ctx context.Context, base *url.URL, noBrowser bool) (string, error) {
	c, err := newClient(base, "", a.opts.Version, a.http)
	if err != nil {
		return "", err
	}
	form := url.Values{"client_id": {deviceClientID}}
	resp, err := c.do(ctx, http.MethodPost, c.url("/oauth/device"), strings.NewReader(form.Encode()),
		http.Header{"Content-Type": {"application/x-www-form-urlencoded"}, "Accept": {"application/json"}}, http.StatusOK)
	if err != nil {
		return "", fmt.Errorf("start the device login: %w", err)
	}
	var dc deviceCode
	err = json.NewDecoder(resp.Body).Decode(&dc)
	_ = resp.Body.Close()
	if err != nil {
		return "", fmt.Errorf("start the device login: %w", err)
	}
	link := dc.VerificationURIComplete
	if link == "" {
		link = dc.VerificationURI
	}
	a.logf("To approve this login, open %s and check the code is %s.\n", link, dc.UserCode)
	if !noBrowser {
		if err := openBrowser(link); err != nil {
			a.logf("Could not open a browser (%v). Open the link yourself.\n", err)
		}
	}
	interval := time.Duration(dc.Interval) * time.Second
	if interval <= 0 {
		interval = 5 * time.Second
	}
	deadline := time.Now().Add(time.Duration(dc.ExpiresIn) * time.Second)
	if dc.ExpiresIn <= 0 {
		deadline = time.Now().Add(15 * time.Minute)
	}
	a.logf("Waiting for approval...\n")
	for {
		if time.Now().After(deadline) {
			return "", authf("the login code expired before it was approved; run hangar login again")
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(interval):
		}
		token, pending, slow, err := pollDeviceToken(ctx, c, dc.DeviceCode)
		if err != nil {
			return "", err
		}
		if slow {
			interval += 5 * time.Second
		}
		if !pending {
			return token, nil
		}
	}
}

// pollDeviceToken asks /oauth/token once. It returns pending while the user
// has not decided yet.
func pollDeviceToken(ctx context.Context, c *client, deviceCode string) (token string, pending, slow bool, err error) {
	form := url.Values{
		"grant_type":  {"urn:ietf:params:oauth:grant-type:device_code"},
		"device_code": {deviceCode},
		"client_id":   {deviceClientID},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url("/oauth/token"), strings.NewReader(form.Encode()))
	if err != nil {
		return "", false, false, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	c.authorise(req)
	resp, err := c.send(req)
	if err != nil {
		return "", false, false, fmt.Errorf("reach %s: %w", c.base.Host, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	var out struct {
		AccessToken string `json:"access_token"`
		Error       string `json:"error"`
	}
	_ = json.Unmarshal(body, &out)
	if resp.StatusCode == http.StatusOK && out.AccessToken != "" {
		return out.AccessToken, false, false, nil
	}
	switch out.Error {
	case "authorization_pending":
		return "", true, false, nil
	case "slow_down":
		return "", true, true, nil
	case "access_denied":
		return "", false, false, authf("the login was denied")
	case "expired_token":
		return "", false, false, authf("the login code expired before it was approved; run hangar login again")
	}
	return "", false, false, apiError(resp.StatusCode, resp.Header, body)
}

// me returns the signed-in user.
func (c *client) me(ctx context.Context) (*gen.Me, error) {
	r, err := c.api.GetMeWithResponse(ctx)
	if err != nil {
		return nil, fmt.Errorf("reach %s: %w", c.base.Host, err)
	}
	if r.StatusCode() == http.StatusNoContent {
		return nil, authf("not signed in: run hangar login, or pass --token")
	}
	if err := check(r.HTTPResponse, r.Body, http.StatusOK); err != nil {
		return nil, err
	}
	if r.JSON200 == nil {
		return nil, fmt.Errorf("the Instance sent no user")
	}
	return r.JSON200, nil
}

func (a *app) logoutCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "logout",
		Short: "Forget the saved Token",
		Long: `Removes the saved Token from the OS keychain and the configuration file.
The Token itself stays valid until you revoke it in the web UI under
Settings, Tokens.`,
		Args: args(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			a := a.with(cmd)
			s, path, err := loadSettings()
			if err != nil {
				return err
			}
			if s.URL == "" {
				if a.json {
					return a.printJSON(logoutResult{})
				}
				a.printf("You are not signed in.\n")
				return nil
			}
			s.forgetToken()
			s.Username = ""
			if err := s.save(path); err != nil {
				return err
			}
			if a.json {
				return a.printJSON(logoutResult{URL: s.URL, SignedOut: true})
			}
			a.printf("Signed out of %s.\n", s.URL)
			return nil
		},
	}
	cmd.Flags().BoolVar(&a.json, "json", false, "print JSON instead of text")
	return cmd
}

// logoutResult is what hangar logout --json prints. SignedOut is false when
// there was no saved login.
type logoutResult struct {
	URL       string `json:"url"`
	SignedOut bool   `json:"signed_out"`
}

func (a *app) whoamiCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "whoami",
		Short: "Show the signed-in user",
		Args:  args(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			a := a.with(cmd)
			c, err := a.client()
			if err != nil {
				return err
			}
			if c.token == "" {
				return authf("not signed in to %s; run hangar login", c.base)
			}
			me, err := c.me(cmd.Context())
			if err != nil {
				return err
			}
			if a.json {
				return a.printJSON(me)
			}
			a.printf("%s on %s\n", me.Username, c.base)
			if me.IsAdmin {
				a.printf("Instance admin\n")
			}
			if len(me.Orgs) > 0 {
				rows := make([][]string, 0, len(me.Orgs))
				for _, o := range me.Orgs {
					rows = append(rows, []string{o.Name, string(o.Role)})
				}
				return a.table([]string{"ORGANISATION", "ROLE"}, rows)
			}
			return nil
		},
	}
	a.clientFlags(cmd)
	return cmd
}

// userHome returns the home directory, for paths such as ~/.ollama.
func userHome() string {
	h, err := os.UserHomeDir()
	if err != nil {
		return "."
	}
	return h
}
