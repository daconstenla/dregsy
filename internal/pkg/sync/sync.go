/*
	Copyright 2020 Alexander Vollschwitz <xelalex@gmx.net>

	Licensed under the Apache License, Version 2.0 (the "License");
	you may not use this file except in compliance with the License.
	You may obtain a copy of the License at

	  http://www.apache.org/licenses/LICENSE-2.0

	Unless required by applicable law or agreed to in writing, software
	distributed under the License is distributed on an "AS IS" BASIS,
	WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
	See the License for the specific language governing permissions and
	limitations under the License.
*/

package sync

import (
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/xelalexv/dregsy/internal/pkg/relays/docker"
	"github.com/xelalexv/dregsy/internal/pkg/relays/skopeo"
	"github.com/xelalexv/dregsy/internal/pkg/tags"
)

//
type Relay interface {
	Prepare() error
	Dispose() error
	Sync(srcRef, srcAuth string, srcSkiptTLSVerify bool,
		trgtRef, trgtAuth string, trgtSkiptTLSVerify bool,
		tags *tags.TagSet, verbose bool) error
}

//
type Sync struct {
	relay    Relay
	shutdown chan bool
	ticks    chan bool
	dryRun   bool
}

//
func New(conf *SyncConfig, dryRun bool) (*Sync, error) {

	sync := &Sync{}

	var relay Relay
	var err error

	switch conf.Relay {

	case docker.RelayID:
		relay, err = docker.NewDockerRelay(
			conf.Docker, log.StandardLogger().WriterLevel(log.DebugLevel))

	case skopeo.RelayID:
		relay = skopeo.NewSkopeoRelay(
			conf.Skopeo, log.StandardLogger().WriterLevel(log.DebugLevel))

	default:
		err = fmt.Errorf("relay type '%s' not supported", conf.Relay)
	}

	if err != nil {
		return nil, fmt.Errorf("cannot create sync relay: %v", err)
	}

	sync.relay = relay
	sync.shutdown = make(chan bool)
	sync.ticks = make(chan bool, 1)
	sync.dryRun = dryRun

	return sync, nil
}

//
func (s *Sync) Shutdown() {
	s.shutdown <- true
	s.WaitForTick()
}

//
func (s *Sync) tick() {
	select {
	case s.ticks <- true:
	default:
	}
}

//
func (s *Sync) WaitForTick() {
	<-s.ticks
}

//
func (s *Sync) Dispose() {
	s.relay.Dispose()
}

//
func (s *Sync) SyncFromConfig(conf *SyncConfig) error {

	if err := s.relay.Prepare(); err != nil {
		return err
	}

	// one-off tasks
	for _, t := range conf.Tasks {
		if t.Interval == 0 {
			s.syncTask(t)
		}
	}

	// periodic tasks
	c := make(chan *Task)
	ticking := false

	for _, t := range conf.Tasks {
		if t.Interval > 0 {
			t.startTicking(c)
			ticking = true
		}
	}

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)

	for ticking {
		log.Info("waiting for next sync task...")
		select {
		case t := <-c: // actual task
			s.syncTask(t)
			s.tick() // send a tick
		case sig := <-sigs: // interrupt signal
			log.WithField("signal", sig).Info("received signal, stopping ...")
			ticking = false
		case <-s.shutdown: // shutdown flagged
			log.Info("shutdown flagged, stopping ...")
			ticking = false
			s.tick() // send a final tick to release shutdown client
		}
	}

	log.Debug("stopping tasks")
	errs := false
	for _, t := range conf.Tasks {
		t.stopTicking()
		errs = errs || t.failed
	}

	if errs {
		return fmt.Errorf(
			"one or more tasks had errors, please see log for details")
	}

	log.Info("all done")
	return nil
}

//
func (s *Sync) syncTask(t *Task) {

	if t.tooSoon() {
		log.WithField("task", t.Name).Info("task fired too soon, skipping")
		return
	}

	log.WithFields(log.Fields{
		"task":   t.Name,
		"source": t.Source.Registry,
		"target": t.Target.Registry}).Info("syncing task")
	t.failed = false

	for _, m := range t.Mappings {

		log.WithFields(log.Fields{"from": m.From, "to": m.To}).Info("mapping")

		if err := t.Source.RefreshAuth(); err != nil {
			log.Error(err)
			t.fail(true)
			continue
		}
		if s.dryRun {
			log.Debug("No need to get auth on the target as is a dry run")
		} else {
			if err := t.Target.RefreshAuth(); err != nil {
				log.Error(err)
				t.fail(true)
				continue
			}
		}

		refs, err := t.mappingRefs(m)
		if err != nil {
			log.Error(err)
			t.fail(true)
			continue
		}

		for _, ref := range refs {

			src := ref[0]
			trgt := ref[1]

			if err := t.ensureTargetExists(trgt); err != nil {
				log.Error(err)
				t.fail(true)
				break
			}

			tags, err := m.tagSet.Expand(func() ([]string, error) {
				return skopeo.ListAllTags(
					src, t.Source.GetAuth(), "", t.Source.SkipTLSVerify)
			})
			if err != nil {
				log.Errorf("error expanding tags: %v", err)
			}

			if s.dryRun {
				log.WithFields(log.Fields{
					"image name": t.Name,
					"list of tags": tags,
					"number of images": len(tags)}).Info("list of tags")
				continue
			}

			if err := s.relay.Sync(src, t.Source.GetAuth(), t.Source.SkipTLSVerify,
				trgt, t.Target.GetAuth(), t.Target.SkipTLSVerify, m.tagSet,
				t.Verbose); err != nil {
				log.Error(err)
				t.fail(true)
			}
		}
	}

	t.lastTick = time.Now()
}
