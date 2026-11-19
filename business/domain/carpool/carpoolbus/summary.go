package carpoolbus

import (
	"context"
	"time"

	"github.com/jroedel/dropin-forms/business/domain/form/formbus"
	"github.com/jroedel/dropin-forms/business/types"
)

// summaryQuiet is how long a car must go unchanged before its driver is told
// who is in it now. Long enough that a rider trying a few cars, or a run of
// sign-ups after an announcement, is one text rather than several; short
// enough that a driver hears the same evening.
const summaryQuiet = 10 * time.Minute

// summarise welcomes the drivers who have not been, and tells each driver
// whose car has changed and since gone quiet who is in it now. Run at the end
// of every pass, so a summary goes out within one pass of being due.
//
// With texting off nothing is recorded either, so that turning it on later
// welcomes every driver then rather than nobody.
func (b *Business) summarise(ctx context.Context, f formbus.Form, t *texts, everyone map[types.ID]person, drivers []person, now time.Time) error {
	if b.cfg.Texter == nil {
		return nil
	}

	told, err := b.cfg.Store.Cars(ctx, f.ID)
	if err != nil {
		return err
	}

	texted := make(map[types.ID]CarTexts, len(told))
	for _, c := range told {
		texted[c.Driver] = c
	}

	var rides []Ride

	for _, d := range drivers {
		c, welcomed := texted[d.sub.ID]
		due := c.Due() && now.Sub(c.ChangedAt) >= summaryQuiet

		if welcomed && !due {
			continue
		}

		// Read once, and only when somebody is to be told.
		if rides == nil {
			if rides, err = b.cfg.Store.ByForm(ctx, f.ID); err != nil {
				return err
			}
		}

		t.summary(d, ridersIn(rides, everyone, d.sub.ID), !welcomed, now)
	}

	return nil
}

// ridersIn is the people riding with a driver, in the order they were put in
// the car.
func ridersIn(rides []Ride, everyone map[types.ID]person, driver types.ID) []person {
	var out []person

	for _, r := range rides {
		if r.Driver != driver {
			continue
		}

		if p, ok := everyone[r.Rider]; ok && p.Riding() {
			out = append(out, p)
		}
	}

	return out
}
