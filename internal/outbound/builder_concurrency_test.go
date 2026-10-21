package outbound

import (
	"fmt"
	"sync"
	"testing"
)

// TestRegisterDetourOutbound_Concurrent verifies that concurrent subscription
// refreshes can register/unregister per-subscription upstream outbounds
// without tripping the Go runtime's concurrent map check.
func TestRegisterDetourOutbound_Concurrent(t *testing.T) {
	b := newTestSingboxBuilder(t)
	defer b.Close()

	const workers = 8
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			tag := fmt.Sprintf("__resin_sub_upstream_%d", i%3)
			other := fmt.Sprintf("__resin_sub_upstream_%d", (i+1)%3)
			for j := 0; j < 30; j++ {
				if err := b.RegisterDetourOutbound(tag, "", "socks5://127.0.0.1:1080"); err != nil {
					t.Errorf("register %s: %v", tag, err)
					return
				}
				b.UnregisterDetourOutbound(other)
			}
		}(i)
	}
	wg.Wait()
}
