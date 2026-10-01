package manager

import (
	"context"
	"log/slog"
	"time"

	"github.com/rytsh/krabby/internal/service/bigpicture"
)

type BigPictureSchedule struct {
	Name  string
	Specs []string
}

func (m *Manager) BigPictureSchedules(ctx context.Context) ([]BigPictureSchedule, error) {
	if m.bigPictures == nil {
		return nil, nil
	}
	pictures, err := m.bigPictures.All(ctx, "*")
	if err != nil {
		return nil, err
	}
	out := []BigPictureSchedule{}
	for _, p := range pictures {
		if len(p.Schedule) > 0 {
			out = append(out, BigPictureSchedule{Name: p.Name, Specs: p.Schedule})
		}
	}
	return out, nil
}

// Re-check the saved schedule when firing so a removed schedule stops source
// disclosure immediately, even before the next cron-set reconciliation tick.
func (m *Manager) TriggerScheduledBigPicture(ctx context.Context, name string) error {
	p, err := m.BigPicture(ctx, name)
	if err != nil {
		return err
	}
	if len(p.Schedule) == 0 {
		return nil
	}
	if m.pictureGenerationLive(name) {
		return nil
	}
	err = m.triggerPictureGeneration(ctx, name, "schedule")
	if err != nil {
		// A scheduled trigger that cannot even queue is otherwise invisible.
		m.recordPictureTriggerFailure(ctx, p, err)
	}
	return err
}

func (m *Manager) recordPictureTriggerFailure(ctx context.Context, p *bigpicture.Picture, err error) {
	run := bigpicture.Run{Trigger: "schedule", Status: bigpicture.RunFailed, Message: err.Error(), At: time.Now().UTC(), ConfigVersion: p.Version, Revision: p.CurrentRevision}
	if recordErr := m.bigPictures.RecordRun(ctx, p.Name, p.StorageID, run); recordErr != nil {
		slog.Warn("record big picture schedule failure", "name", p.Name, "error", recordErr)
	}
}
