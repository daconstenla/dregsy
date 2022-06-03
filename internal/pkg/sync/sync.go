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

	"github.com/xelalexv/dregsy/internal/pkg/relays"
	"github.com/xelalexv/dregsy/internal/pkg/relays/docker"
	"github.com/xelalexv/dregsy/internal/pkg/relays/skopeo"
	"github.com/xelalexv/dregsy/internal/pkg/util"
)

//
type Relay interface {
	Prepare() error
	Dispose() error
	Sync(opt *relays.SyncOptions) error
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
		if err = conf.ValidateSupport(&docker.Support{}); err == nil {
			relay, err = docker.NewDockerRelay(
				conf.Docker, log.StandardLogger().WriterLevel(log.DebugLevel), dryRun)
		}

	case skopeo.RelayID:
		if err = conf.ValidateSupport(&skopeo.Support{}); err == nil {
			relay = skopeo.NewSkopeoRelay(
				conf.Skopeo, log.StandardLogger().WriterLevel(log.DebugLevel), dryRun)
		}

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
func (s *Sync) SyncFromConfig(conf *SyncConfig, taskFilter string) error {

	if taskFilter == "" {
		taskFilter = ".*"
	}

	tf, err := util.NewRegex(taskFilter)
	if err != nil {
		return fmt.Errorf("invalid task filter: %v", err)
	}

	if err := s.relay.Prepare(); err != nil {
		return err
	}

	// one-off tasks
	for _, t := range conf.Tasks {
		if t.Interval == 0 && tf.Matches(t.Name) {
			s.syncTask(t)
		}
	}

	// periodic tasks
	c := make(chan *Task)
	ticking := false

	for _, t := range conf.Tasks {
		if t.Interval > 0 && tf.Matches(t.Name) {
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
		if err := t.Target.RefreshAuth(); err != nil {
			log.Error(err)
			t.fail(true)
			continue
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

			if !s.dryRun {
				if err := t.ensureTargetExists(trgt); err != nil {
					log.Error(err)
					t.fail(true)
					break
				}
			}

			/*
				FIXME

				- Push dry run handling into relays. The code for expanding source
				  tag lists is already present there, and auth & TLS verify work
				  correctly. This would additionally exercises all the mechanics
				  involved, except the actual sync.

				- Target tag lists could then be retrieved via a helper object.
				  This could actually be set on the relay instead of just a dry
				  run flag.

				- When retrieving target tag list, we need to consider the case
				  where the target repo does not yet exist. This is easy since
				  all expanded source tags would then apply. We just need to
				  handle it properly.

				- Improve output, the dry-run log output seems a bit hard to
				  read. Maybe we could create a JSON/YAML/diff file or whatever
				  instead of writing to log. That could then also be processed
				  with generic tools.

				- Make all interval tasks one-off. We may not want the dry-run
				  to run indefinitely.
			*/
			if s.dryRun {
				log.Debugf("dry-run, obtaining information about [%s] docker tags from [%s] and [%s]", t.Name, src, trgt)
				tags_source, err := m.tagSet.Expand(func() ([]string, error) {
					return skopeo.ListAllTags(
						src, t.Source.GetAuth(), "", t.Source.SkipTLSVerify)
				})
				if err != nil {
					log.Errorf("dry-run, error expanding tags from source: %v", err)
				}

				tags_target, err := skopeo.ListAllTags(
					trgt, t.Target.GetAuth(), "", t.Target.SkipTLSVerify)

				if err != nil {
					log.Errorf("dry-run, error expanding tags from target: %v", err)
				}

				log.WithFields(log.Fields{
					"image name":                         t.Name,
					"tags on target registry":            tags_target,
					"candidate tags be synced":           tags_source,
					"number of candidate tags be synced": len(tags_source),
					"tags available but not synced":      util.DiffBetweenLists(tags_target, tags_source),
					"number of tags on target":           len(tags_target),
					"not synced tags":                    util.DiffBetweenLists(tags_source, tags_target),
				}).Info("dry-run, list of tags")
				continue
			}

			if err := s.relay.Sync(&relays.SyncOptions{
				SrcRef:            src,
				SrcAuth:           t.Source.GetAuth(),
				SrcSkipTLSVerify:  t.Source.SkipTLSVerify,
				TrgtRef:           trgt,
				TrgtAuth:          t.Target.GetAuth(),
				TrgtSkipTLSVerify: t.Target.SkipTLSVerify,
				Tags:              m.tagSet,
				Platform:          m.Platform,
				Verbose:           t.Verbose}); err != nil {
				log.Error(err)
				t.fail(true)
			}
		}
	}

	t.lastTick = time.Now()
}
