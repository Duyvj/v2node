package conf

import (
	"context"
	"fmt"
	"path/filepath"
	"time"

	"github.com/fsnotify/fsnotify"
)

// Watch signals a serialized reload; it never mutates the running config.
func (p *Conf) Watch(filePath string, reload func()) error {
	target, err := filepath.Abs(filePath)
	if err != nil {
		return err
	}
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	if err = watcher.Add(filepath.Dir(target)); err != nil {
		watcher.Close()
		return fmt.Errorf("watch config: %w", err)
	}
	p.CloseWatch()
	ctx, cancel := context.WithCancel(context.Background())
	p.watchCancel = cancel
	p.watchDone = make(chan struct{})
	done := p.watchDone
	go func() {
		defer close(done)
		defer watcher.Close()
		var timer *time.Timer
		var tick <-chan time.Time
		defer func() {
			if timer != nil {
				timer.Stop()
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
				if filepath.Clean(event.Name) != target || event.Op&(fsnotify.Write|fsnotify.Create|fsnotify.Rename) == 0 {
					continue
				}
				if timer == nil {
					timer = time.NewTimer(500 * time.Millisecond)
				} else {
					timer.Reset(500 * time.Millisecond)
				}
				tick = timer.C
			case _, ok := <-watcher.Errors:
				if !ok {
					return
				}
			case <-tick:
				tick = nil
				reload()
			}
		}
	}()
	return nil
}
func (p *Conf) CloseWatch() {
	if p.watchCancel != nil {
		p.watchCancel()
		<-p.watchDone
		p.watchCancel = nil
		p.watchDone = nil
	}
}
