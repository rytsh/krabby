package scheduler

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/rytsh/krabby/internal/service/manager"
	"github.com/worldline-go/hardloop"
)

func (s *scheduler) reconcilePictures(ctx context.Context) {
	schedules, err := s.mgr.BigPictureSchedules(ctx)
	if err != nil {
		slog.Warn("load big picture schedules failed")
		return
	}
	data, _ := json.Marshal(schedules)
	sig := string(data)
	if sig == s.pictureSig && s.pictureCron != nil {
		return
	}
	crons := s.buildPictureCrons(schedules)
	if len(crons) == 0 {
		s.stopPictures()
		s.pictureSig = sig
		return
	}
	job, err := hardloop.NewCron(crons...)
	if err != nil {
		slog.Warn("build big picture schedules failed")
		return
	}
	if err := job.Start(ctx); err != nil {
		slog.Warn("start big picture schedules failed")
		return
	}
	s.stopPictures()
	s.pictureCron, s.pictureSig = job, sig
}

func (s *scheduler) buildPictureCrons(schedules []manager.BigPictureSchedule) []hardloop.Cron {
	crons := []hardloop.Cron{}
	for _, sc := range schedules {
		if len(sc.Specs) == 0 {
			continue
		}
		name := sc.Name
		crons = append(crons, hardloop.Cron{Name: "bigpicture-update:" + name, Specs: sc.Specs, Func: func(ctx context.Context) error { return s.mgr.TriggerScheduledBigPicture(ctx, name) }})
	}
	return crons
}

func (s *scheduler) stopPictures() {
	if s.pictureCron != nil {
		s.pictureCron.Stop()
		s.pictureCron = nil
	}
}
