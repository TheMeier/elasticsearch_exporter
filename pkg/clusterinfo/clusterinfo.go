package clusterinfo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"time"

	"github.com/go-kit/kit/log"
	"github.com/go-kit/kit/log/level"
	"github.com/prometheus/client_golang/prometheus"
)

const (
	namespace = "elasticsearch"
	subsystem = "custerinfo"
)

var (
	// ErrConsumerAlreadyRegistered is returned if a consumer is already registered
	ErrConsumerAlreadyRegistered = errors.New("consumer already registered")
)

type consumer interface {
	// ClusterLabelUpdates returns a pointer to channel for cluster label updates
	ClusterLabelUpdates() *chan *Response
	// String implements the stringer interface
	String() string
}

// Retriever periodically gets the cluster info from the / endpoint end
// sends it to all registered consumer channels
type Retriever struct {
	consumerChannels      map[string]*chan *Response
	logger                log.Logger
	client                *http.Client
	url                   *url.URL
	interval              time.Duration
	sync                  chan struct{}
	versionMetric         *prometheus.GaugeVec
	up                    *prometheus.GaugeVec
	lastUpstreamSuccessTs *prometheus.GaugeVec
	lastUpstreamErrorTs   *prometheus.GaugeVec
}

// New creates a new Retriever
func New(logger log.Logger, client *http.Client, u *url.URL, interval time.Duration) *Retriever {
	return &Retriever{
		consumerChannels: make(map[string]*chan *Response),
		logger:           logger,
		client:           client,
		url:              u,
		interval:         interval,
		sync:             make(chan struct{}, 1),
		versionMetric: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Name: prometheus.BuildFQName(namespace, subsystem, "version_info"),
				Help: "Constant metric with ES version information as labels",
			},
			[]string{
				"cluster",
				"cluster_uuid",
				"build_date",
				"build_hash",
				"version",
				"lucene_version",
			},
		),
		up: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Name: prometheus.BuildFQName(namespace, subsystem, "up"),
				Help: "Up metric for the cluster info collector",
			},
			[]string{"url"},
		),
		lastUpstreamSuccessTs: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Name: prometheus.BuildFQName(namespace, subsystem, "last_retrieval_success_ts"),
				Help: "Timestamp of the last successful cluster info retrieval",
			},
			[]string{"url"},
		),
		lastUpstreamErrorTs: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Name: prometheus.BuildFQName(namespace, subsystem, "last_retrieval_failure_ts"),
				Help: "Timestamp of the last failed cluster info retrieval",
			},
			[]string{"url"},
		),
	}
}

// Describe implements the prometheus.Collector interface
func (r *Retriever) Describe(ch chan<- *prometheus.Desc) {
	r.versionMetric.Describe(ch)
	r.up.Describe(ch)
	r.lastUpstreamSuccessTs.Describe(ch)
	r.lastUpstreamErrorTs.Describe(ch)
}

// Collect implements the prometheus.Collector interface
func (r *Retriever) Collect(ch chan<- prometheus.Metric) {
	r.versionMetric.Collect(ch)
	r.up.Collect(ch)
	r.lastUpstreamSuccessTs.Collect(ch)
	r.lastUpstreamErrorTs.Collect(ch)
}

func (r *Retriever) updateMetrics(res *Response) {
	level.Debug(r.logger).Log("msg", "updating cluster info metrics")
	// scrape failed, response is nil
	if res == nil {
		r.up.WithLabelValues(r.url.String()).Set(0.0)
		r.lastUpstreamErrorTs.WithLabelValues(r.url.String()).Set(float64(time.Now().Unix()))
		return
	}
	r.up.WithLabelValues(r.url.String()).Set(1.0)
	r.versionMetric.WithLabelValues(
		res.ClusterName,
		res.ClusterUUID,
		res.Version.BuildDate.String(),
		res.Version.BuildHash,
		res.Version.Number.String(),
		res.Version.LuceneVersion.String(),
	)
	r.lastUpstreamSuccessTs.WithLabelValues(r.url.String()).Set(float64(time.Now().Unix()))
}

// Update triggers an external cluster info label update
func (r *Retriever) Update() {
	r.sync <- struct{}{}
}

// RegisterConsumer registers a consumer for cluster info updates
func (r *Retriever) RegisterConsumer(c consumer) error {
	if _, registered := r.consumerChannels[c.String()]; registered {
		return ErrConsumerAlreadyRegistered
	}
	r.consumerChannels[c.String()] = c.ClusterLabelUpdates()
	return nil
}

// Run starts the update loop and periodically queries the / endpoint
// The update loop is terminated upon ctx cancellation. The call blocks until the first
// call to the cluster info endpoint was successful
func (r *Retriever) Run(ctx context.Context) error {
	initial := true
	// start update routine
	go func(ctx context.Context) {
		for {
			select {
			case <-ctx.Done():
				_ = level.Info(r.logger).Log(
					"msg", "context cancelled, exiting cluster info update loop",
					"err", ctx.Err(),
				)
				return
			case <-r.sync:
				_ = level.Info(r.logger).Log(
					"msg", "providing consumers with updated cluster info label",
				)
				res, err := r.fetchAndDecodeClusterInfo()
				if err != nil {
					_ = level.Error(r.logger).Log(
						"msg", "failed to retrieve cluster info from ES",
						"err", err,
					)
					r.updateMetrics(nil)
					continue
				}
				r.updateMetrics(res)
				for name, consumerCh := range r.consumerChannels {
					_ = level.Debug(r.logger).Log(
						"msg", "sending update",
						"consumer", name,
						"res", fmt.Sprintf("%+v", res),
					)
					*consumerCh <- res
				}
				initial = false
			}
		}
	}(ctx)
	// trigger initial cluster info call
	_ = level.Info(r.logger).Log(
		"msg", "triggering initial cluster info call",
	)
	r.sync <- struct{}{}

	// start a ticker routine
	go func(ctx context.Context) {
		if r.interval <= 0 {
			_ = level.Info(r.logger).Log(
				"msg", "no periodic cluster info label update requested",
			)
			return
		}
		ticker := time.NewTicker(r.interval)
		for {
			select {
			case <-ctx.Done():
				_ = level.Info(r.logger).Log(
					"msg", "context cancelled, exiting cluster info trigger loop",
					"err", ctx.Err(),
				)
				return
			case <-ticker.C:
				_ = level.Debug(r.logger).Log(
					"msg", "triggering periodic update",
				)
				r.sync <- struct{}{}
			}
		}
	}(ctx)
	// block until the first retrieval was successful
	retryTicker := time.NewTicker(1 * time.Second)
	tries := 0
	for range retryTicker.C {
		if !initial {
			return nil
		}
		if tries > 10 {
			return errors.New("initial cluster info call timed out")
		}
		tries++
	}
	return nil
}

func (r *Retriever) fetchAndDecodeClusterInfo() (*Response, error) {
	var response *Response
	u := *r.url
	u.Path = path.Join(r.url.Path, "/")

	res, err := r.client.Get(u.String())
	if err != nil {
		_ = level.Error(r.logger).Log(
			"msg", "failed to get cluster info",
			"err", err,
		)
		return nil, err
	}

	defer func() {
		err = res.Body.Close()
		if err != nil {
			_ = level.Warn(r.logger).Log(
				"msg", "failed to close http.Client",
				"err", err,
			)
		}
	}()

	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP Request failed with code %d", res.StatusCode)
	}

	if err := json.NewDecoder(res.Body).Decode(&response); err != nil {
		return nil, err
	}

	return response, nil
}
