package gitutil

import (
	"context"
	"os/exec"
	"sync"
	"time"
)

// ChildCPU accounts the CPU of the Git processes started under one context,
// so a long-lived process running several reads at once can give each read
// the CPU of its own children exactly.
type ChildCPU struct {
	mu   sync.Mutex
	cmds []*exec.Cmd
}

type childCPUKey struct{}

// abandonedCmds are commands returned from while another goroutine still
// waits for them; Total never reads their state, which that wait writes.
var abandonedCmds sync.Map

func abandonChildCPU(cmd *exec.Cmd) { abandonedCmds.Store(cmd, true) }

// WithChildCPU returns a context under which every Git process this package
// starts is accounted to the returned ChildCPU.
func WithChildCPU(ctx context.Context) (context.Context, *ChildCPU) {
	account := &ChildCPU{}
	return context.WithValue(ctx, childCPUKey{}, account), account
}

func trackChildCPU(ctx context.Context, cmd *exec.Cmd) {
	if account, _ := ctx.Value(childCPUKey{}).(*ChildCPU); account != nil {
		account.mu.Lock()
		account.cmds = append(account.cmds, cmd)
		account.mu.Unlock()
	}
}

// Total is the CPU of the accounted processes that have exited and been
// waited for. A read calls it once its own Git processes have ended (it waits
// for each before it answers); one a stop gave up waiting for is left out.
func (account *ChildCPU) Total() (user, system time.Duration) {
	account.mu.Lock()
	defer account.mu.Unlock()
	for _, cmd := range account.cmds {
		if _, abandoned := abandonedCmds.Load(cmd); abandoned {
			continue
		}
		if state := cmd.ProcessState; state != nil {
			user += state.UserTime()
			system += state.SystemTime()
		}
	}
	return user, system
}
