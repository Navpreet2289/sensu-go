package backend

import (
	"context"
	"crypto/md5"
	"encoding/binary"
	"encoding/json"
	"errors"
	"sync"
	"time"

	log "github.com/Sirupsen/logrus"

	"github.com/coreos/etcd/clientv3"
	"github.com/sensu/sensu-go/backend/messaging"
	"github.com/sensu/sensu-go/backend/store"
	"github.com/sensu/sensu-go/types"
)

// CheckScheduler is responsible for looping and publishing check requests for
// a given check.
type CheckScheduler struct {
	MessageBus messaging.MessageBus
	Store      store.Store
	Check      *types.Check
	Stopped    bool
	stop       chan struct{}
}

// Start the scheduling loop
func (s *CheckScheduler) Start() error {
	s.Stopped = false
	s.stop = make(chan struct{})
	sum := md5.Sum([]byte(s.Check.Name))
	splayHash, n := binary.Uvarint(sum[0:7])
	if n < 0 {
		return errors.New("check hashing failed")
	}

	go func() {
		now := uint64(time.Now().UnixNano())
		// (splay_hash - current_time) % (interval * 1000) / 1000
		nextExecution := (splayHash - now) % (uint64(s.Check.Interval) * uint64(1000))
		timer := time.NewTimer(time.Duration(nextExecution) * time.Millisecond)
		for {
			select {
			case <-timer.C:
				check, err := s.Store.GetCheckByName(s.Check.Name)
				if err != nil {
					log.Error("error getting check from store: ", err.Error())
					// TODO(grep): what do we do when we cannot talk to the store?
					continue
				}

				if check == nil {
					// The check has been deleted, and there was no error talking to etcd.
					timer.Stop()
					close(s.stop)
					return
				}

				// update our pointer to the check
				s.Check = check

				timer.Reset(time.Duration(time.Second * time.Duration(s.Check.Interval)))
				for _, sub := range s.Check.Subscriptions {
					evt := &types.Event{
						Timestamp: time.Now().Unix(),
						Check:     s.Check,
					}
					evtBytes, err := json.Marshal(evt)
					if err != nil {
						log.Error("error marshalling check in scheduler: ", err.Error())
						continue
					}

					if err := s.MessageBus.Publish(sub, evtBytes); err != nil {
						log.Error("error publishing check request: ", err.Error())
					}
				}
			case <-s.stop:
				timer.Stop()
				return
			}
		}
	}()
	return nil
}

// Stop stops the CheckScheduler
func (s *CheckScheduler) Stop() {
	if s.stop != nil {
		close(s.stop)
	}
}

func newSchedulerFromCheck(s store.Store, bus messaging.MessageBus, check *types.Check) *CheckScheduler {
	scheduler := &CheckScheduler{
		MessageBus: bus,
		Store:      s,
		Check:      check,
	}
	return scheduler
}

// Checker is responsible for managing check timers and publishing events to
// a messagebus
type Checker struct {
	Client     *clientv3.Client
	Store      store.Store
	MessageBus messaging.MessageBus

	schedulers      map[string]*CheckScheduler
	schedulersMutex *sync.Mutex
	watcher         clientv3.Watcher
	wg              *sync.WaitGroup
	errChan         chan error
	shutdown        chan struct{}
}

// Start the Checker.
func (c *Checker) Start() error {
	if c.Client == nil {
		return errors.New("no etcd client available")
	}

	if c.Store == nil {
		return errors.New("no store available")
	}

	c.schedulers = map[string]*CheckScheduler{}
	c.schedulersMutex = &sync.Mutex{}

	c.errChan = make(chan error, 1)

	// The reconciler and the watchers have to be a little coordinated. We start
	// the watcher first, so that we don't miss any checks that are created
	// during our initial reconciliation phase.
	c.wg = &sync.WaitGroup{}
	c.wg.Add(2)
	c.startWatcher()

	c.shutdown = make(chan struct{})
	c.reconcile()
	c.startReconciler()
	return nil
}

func (c *Checker) reconcile() error {
	checks, err := c.Store.GetChecks()
	if err != nil {
		return err
	}

	for _, check := range checks {
		c.schedulersMutex.Lock()
		if _, ok := c.schedulers[check.Name]; !ok {
			scheduler := newSchedulerFromCheck(c.Store, c.MessageBus, check)
			err = scheduler.Start()
			if err != nil {
				c.schedulersMutex.Unlock()
				return err
			}
			c.schedulers[check.Name] = scheduler
		}
		c.schedulersMutex.Unlock()
	}
	return nil
}

func (c *Checker) startReconciler() {
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		for {
			select {
			case <-c.shutdown:
				ticker.Stop()
				c.wg.Done()
				return
			case <-ticker.C:
				c.reconcile()
			}
		}
	}()
}

// All the watcher has to do is make sure that we have schedulers for any checks
// that are created. Once the scheduler is in place, it will self manage.
func (c *Checker) startWatcher() {
	go func() {
		for {
			select {
			case <-c.shutdown:
				c.wg.Done()
				return
			default:
				// TODO(grep): this should probably come from our own factory. have a
				// WatchFactory interface that takes a *clientv3.Client and returns a
				// clientv3.Watcher (interface). Then we can have the etcd factory and
				// the testing factory so we can do unit testing.
				c.watcher = clientv3.NewWatcher(c.Client)
			}
			for resp := range c.watcher.Watch(
				context.TODO(),
				"/sensu.io/checks",
				clientv3.WithPrefix(),
				clientv3.WithFilterDelete(),
				clientv3.WithFilterPut(),
				clientv3.WithCreatedNotify(),
			) {
				for _, ev := range resp.Events {
					c.schedulersMutex.Lock()
					check := &types.Check{}
					err := json.Unmarshal(ev.Kv.Value, check)
					if err != nil {
						log.Error("error unmarshalling check \"%s\": %s", string(ev.Kv.Value), err.Error())
						c.schedulersMutex.Unlock()
						continue
					}
					scheduler := newSchedulerFromCheck(c.Store, c.MessageBus, check)
					c.schedulers[check.Name] = scheduler
					err = scheduler.Start()
					if err != nil {
						log.Error("error starting scheduler for check: ", check.Name)
						c.schedulersMutex.Unlock()
					}
					c.schedulersMutex.Unlock()
				}
			}
			// TODO(greg): exponential backoff
			time.Sleep(1 * time.Second)
		}
	}()
}

// Stop the Checker.
func (c *Checker) Stop() error {
	close(c.shutdown)
	c.watcher.Close()
	// let the event queue drain so that we don't panic inside the loop.
	// TODO(greg): get ride of this dependency.
	c.wg.Wait()
	close(c.errChan)
	return nil
}

// Status returns the health of the Checker.
func (c *Checker) Status() error {
	return nil
}

// Err returns a channel on which to listen for terminal errors.
func (c *Checker) Err() <-chan error {
	return c.errChan
}
