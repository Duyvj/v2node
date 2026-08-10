package conf

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/fsnotify/fsnotify"
)

func (p *Conf) Watch(filePath string, reload func()) error {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("new watcher error: %s", err)
	}
	// Add before starting the goroutine so every setup error closes the file
	// descriptor instead of leaving an unreachable watcher behind.
	if err := watcher.Add(filePath); err != nil {
		_ = watcher.Close()
		return fmt.Errorf("watch file error: %s", err)
	}

	p.CloseWatch()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	p.watchMu.Lock()
	p.watchCancel = cancel
	p.watchDone = done
	p.watchMu.Unlock()

	go func() {
		defer close(done)
		defer watcher.Close()
		var debounce *time.Timer
		var debounceC <-chan time.Time
		defer func() {
			if debounce != nil {
				debounce.Stop()
			}
		}()
		for {
			select {
			case <-ctx.Done():
				return
			case event, ok := <-watcher.Events:
				if !ok {
					return
				}
				if event.Has(fsnotify.Chmod) {
					continue
				}
				if debounce == nil {
					debounce = time.NewTimer(5 * time.Second)
				} else {
					if !debounce.Stop() {
						select {
						case <-debounce.C:
						default:
						}
					}
					debounce.Reset(5 * time.Second)
				}
				debounceC = debounce.C
			case <-debounceC:
				debounceC = nil
				log.Println("config file changed, restarting...")
				reload()
			case watchErr, ok := <-watcher.Errors:
				if !ok {
					return
				}
				if watchErr != nil {
					log.Printf("file watcher error: %s", watchErr)
				}
			}
		}
	}()
	return nil
}

func (p *Conf) CloseWatch() {
	p.watchMu.Lock()
	cancel := p.watchCancel
	done := p.watchDone
	p.watchCancel = nil
	p.watchDone = nil
	p.watchMu.Unlock()
	if cancel != nil {
		cancel()
	}
	if done != nil {
		<-done
	}
}
