package agentfw

import (
	"context"
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
	auditor := NewAuditor(os.Stdout)
	ks := NewKillSwitch(killSwitchFile)
	rl := NewRateLimiter(policy.RequestsPerMinute, policy.DataBudgetMB)

	var core http.Handler
	if policy.Upstream != "" {
		rp, err := NewReverseProxy(policy.Upstream, policy, auditor)
		if err != nil {
			return err
		}
		rp.scanner.KillSwitch = ks
		core = rp
	} else {
		px := NewProxy(policy, auditor)
		px.scanner.KillSwitch = ks
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

	// Admin server (kill switch endpoint).
	adminSrv := &http.Server{
		Addr:        adminAddr,
		Handler:     ks.AdminHandler(),
		ReadTimeout: 5 * time.Second,
	}

	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shutCtx)    //nolint:errcheck
		adminSrv.Shutdown(shutCtx) //nolint:errcheck
	}()

	go adminSrv.ListenAndServe() //nolint:errcheck
	return srv.ListenAndServe()
}
