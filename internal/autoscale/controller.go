package autoscale

import (
	"context"
	"log/slog"
	"time"
)

// Demand is the queue-depth signal the controller scales against.
type Demand interface {
	CountQueued(ctx context.Context) (int, error)
}

// Config bounds and paces the autoscaler. The defaults are deliberately
// conservative: a self-replicating system must never fork-bomb, so Max is
// a HARD cap and scale-up is gated by a cooldown so it ramps one worker at
// a time rather than bursting.
type Config struct {
	Min               int           // never go below this many managed workers (default 0 = scale to zero)
	Max               int           // HARD cap: never spawn beyond this (default 4)
	QueuePerWorker    int           // target queued runs per worker before adding one (default 20)
	ScaleUpCooldown   time.Duration // minimum time between spawns (default 30s)
	ScaleDownCooldown time.Duration // queue must stay empty this long before removing a worker (default 2m)
	Interval          time.Duration // control loop tick (default 15s)
}

func (c *Config) applyDefaults() {
	if c.Max <= 0 {
		c.Max = 4
	}
	if c.Min < 0 {
		c.Min = 0
	}
	if c.Min > c.Max {
		c.Min = c.Max
	}
	if c.QueuePerWorker <= 0 {
		c.QueuePerWorker = 20
	}
	if c.ScaleUpCooldown <= 0 {
		c.ScaleUpCooldown = 30 * time.Second
	}
	if c.ScaleDownCooldown <= 0 {
		c.ScaleDownCooldown = 2 * time.Minute
	}
	if c.Interval <= 0 {
		c.Interval = 15 * time.Second
	}
}

// Controller grows/shrinks a worker pool to track queue depth. Run it on
// ONE instance only (the leader) so there is a single autoscaler.
type Controller struct {
	cfg     Config
	spawner Spawner
	demand  Demand
	log     *slog.Logger
	now     func() time.Time

	lastScaleUp time.Time
	lastBusy    time.Time
}

// New builds a controller.
func New(cfg Config, spawner Spawner, demand Demand, log *slog.Logger) *Controller {
	cfg.applyDefaults()
	if log == nil {
		log = slog.Default()
	}
	return &Controller{cfg: cfg, spawner: spawner, demand: demand, log: log, now: time.Now}
}

// Run blocks, scaling the pool every Interval until ctx is cancelled, then
// drains every worker it spawned.
func (c *Controller) Run(ctx context.Context) {
	c.log.Info("autoscale: controller started",
		"min", c.cfg.Min, "max", c.cfg.Max, "queue_per_worker", c.cfg.QueuePerWorker,
		"scale_up_cooldown", c.cfg.ScaleUpCooldown.String(), "scale_down_cooldown", c.cfg.ScaleDownCooldown.String())
	c.lastBusy = c.now()
	// Bring the pool up to Min immediately.
	for c.spawner.Running() < c.cfg.Min {
		if _, err := c.spawner.Spawn(ctx); err != nil {
			c.log.Warn("autoscale: spawn (min) failed", "err", err)
			break
		}
	}
	t := time.NewTicker(c.cfg.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			c.log.Info("autoscale: draining managed workers", "count", c.spawner.Running())
			c.spawner.StopAll(context.Background())
			return
		case <-t.C:
			c.tick(ctx)
		}
	}
}

func (c *Controller) tick(ctx context.Context) {
	queued, err := c.demand.CountQueued(ctx)
	if err != nil {
		c.log.Warn("autoscale: queue probe failed", "err", err)
		return
	}
	running := c.spawner.Running()
	desired := c.desiredWorkers(queued)
	now := c.now()
	if queued > 0 {
		c.lastBusy = now
	}

	switch {
	case running < desired:
		// Scale up ONE worker, paced by the cooldown so even sustained load
		// ramps gradually and never bursts past Max.
		if now.Sub(c.lastScaleUp) >= c.cfg.ScaleUpCooldown {
			if id, err := c.spawner.Spawn(ctx); err != nil {
				c.log.Warn("autoscale: scale-up spawn failed", "err", err)
			} else {
				c.lastScaleUp = now
				c.log.Info("autoscale: scaled up", "id", id, "queued", queued, "running", running+1, "desired", desired, "max", c.cfg.Max)
			}
		}
	case running > desired && running > c.cfg.Min:
		// Scale down ONE worker, but only after the queue has stayed empty
		// for ScaleDownCooldown so we don't flap on brief lulls.
		if now.Sub(c.lastBusy) >= c.cfg.ScaleDownCooldown {
			ids := c.spawner.IDs()
			if len(ids) > 0 {
				_ = c.spawner.Stop(ctx, ids[len(ids)-1])
				c.log.Info("autoscale: scaled down", "queued", queued, "running", running-1, "desired", desired, "min", c.cfg.Min)
			}
		}
	}
}

// desiredWorkers is ceil(queued / QueuePerWorker), clamped to [Min, Max].
// An empty queue yields Min (scale to zero when Min is 0).
func (c *Controller) desiredWorkers(queued int) int {
	d := (queued + c.cfg.QueuePerWorker - 1) / c.cfg.QueuePerWorker
	if d < c.cfg.Min {
		d = c.cfg.Min
	}
	if d > c.cfg.Max {
		d = c.cfg.Max
	}
	return d
}
