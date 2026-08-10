package agentfw

import (
	"context"
	"log"
	"net/http"
	"os"
	"time"
)

const killSwitchFile = "/etc/agentfw/killswitch"

// Serve starts the proxy and blocks until ctx is cancelled.
// If policy.Upstream is set it runs as a reverse proxy in front of the LLM;
// otherwise it runs as a forward proxy (agents set HTTP_PROXY).
func Serve(ctx context.Context, addr, adminAddr, policyPath string) error {
	policy, err := LoadPolicy(policyPath)
	if err != nil && !os.IsNotExist(err) {
		return err
	}

	// Load community rule bundle (no-op if file absent).
	rulesPath := policy.RulesPath
	if rulesPath == "" {
		rulesPath = "/etc/agentfw/rules.yaml"
	}
	if err := LoadRules(rulesPath); err != nil {
		log.Printf("agentfw: rules load warning: %v", err)
	}

	auditor := NewAuditor(os.Stdout)

	// Ed25519 signed receipts (optional).
	keyPath := policy.SigningKeyPath
	if keyPath == "" {
		keyPath = "/etc/agentfw/signing.key"
	}
	if signer, err := NewSigner(keyPath); err == nil {
		auditor.WithSigner(signer)
	} else {
		log.Printf("agentfw: signing disabled: %v", err)
	}

	ks := NewKillSwitch(killSwitchFile)
	rl := NewRateLimiter(policy.RequestsPerMinute, policy.DataBudgetMB)
	sessions := NewSessionStore()

	newScanner := func(p Policy) *Scanner {
		return &Scanner{Policy: p, Auditor: auditor, KillSwitch: ks, Sessions: sessions}
	}

	var core http.Handler
	if policy.Upstream != "" {
		rp, err := NewReverseProxy(policy.Upstream, policy, auditor)
		if err != nil {
			return err
		}
		rp.scanner = newScanner(policy)
		core = rp
	} else {
		px := NewProxy(policy, auditor)
		px.scanner = newScanner(policy)
		if policy.MITMEnabled {
			m, err := LoadMITM(policy.MITMCACert, policy.MITMCAKey)
			if err != nil {
				return err
			}
			px.WithMITM(m)
			log.Printf("agentfw: MITM enabled (ca=%s)", policy.MITMCACert)
		}
		core = px
	}

	handler := rl.Middleware(core, auditor)

	srv := &http.Server{
		Addr:         addr,
		Handler:      handler,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 60 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	adminSrv := &http.Server{
		Addr:        adminAddr,
		Handler:     adminMux(ks),
		ReadTimeout: 5 * time.Second,
	}

	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shutCtx)      //nolint:errcheck
		adminSrv.Shutdown(shutCtx) //nolint:errcheck
	}()

	go adminSrv.ListenAndServe() //nolint:errcheck
	return srv.ListenAndServe()
}
