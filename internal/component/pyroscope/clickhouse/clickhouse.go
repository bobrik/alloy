package clickhouse

import (
	"bytes"
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/google/pprof/profile"
	"github.com/grafana/alloy/internal/component"
	"github.com/grafana/alloy/internal/component/pyroscope"
	"github.com/grafana/alloy/internal/component/pyroscope/util/glue"
	"github.com/grafana/alloy/internal/featuregate"
	"github.com/prometheus/common/model"
	"github.com/prometheus/prometheus/model/labels"
)

func init() {
	component.Register(component.Registration{
		Name:      "pyroscope.clickhouse",
		Stability: featuregate.StabilityGenerallyAvailable,
		Args:      Arguments{},
		Exports:   Exports{},
		Build: func(o component.Options, c component.Arguments) (component.Component, error) {
			args := c.(Arguments)

			component, err := New(args, func(exports Exports) {
				o.OnStateChange(exports)
			})
			if err != nil {
				return nil, err
			}

			return &glue.GenericComponentGlue[Arguments]{Impl: component}, nil
		},
	})
}

// Arguments represents the input state of the pyroscope.write
// component.
type Arguments struct {
	ExternalLabels map[string]string `alloy:"external_labels,attr,optional"`
}

// Exports are the set of fields exposed by the pyroscope.write component.
type Exports struct {
	Receiver pyroscope.Appendable `alloy:"receiver,attr"`
}

type Component struct {
	cfg           Arguments
	onStateChange func(Exports)
}

func New(cfg Arguments, onStateChange func(Exports)) (*Component, error) {
	receiver, err := newReceiver(cfg)
	if err != nil {
		return nil, err
	}

	// Immediately export the receiver
	onStateChange(Exports{Receiver: receiver})

	return &Component{cfg: cfg, onStateChange: onStateChange}, nil
}

// Run implements Component.
func (c *Component) Run(ctx context.Context) error {
	<-ctx.Done()
	return ctx.Err()
}

// Update implements Component.
func (c *Component) Update(newConfig Arguments) error {
	c.cfg = newConfig
	receiver, err := newReceiver(newConfig)
	if err != nil {
		return err
	}
	c.onStateChange(Exports{Receiver: receiver})
	return nil
}

type receiver struct {
	cfg Arguments
	db  clickhouse.Conn
}

func newReceiver(cfg Arguments) (*receiver, error) {
	db, err := clickhouse.Open(&clickhouse.Options{
		Addr: []string{"127.0.0.1:9000"},
		// Settings: clickhouse.Settings{"flatten_nested": 0},
	})
	if err != nil {
		return nil, err
	}
	return &receiver{cfg: cfg, db: db}, nil
}

// Push implements the PusherServiceClient interface.
// func (r *receiver) Push(
// 	ctx context.Context,
// 	req *connect.Request[pushv1.PushRequest],
// ) (*connect.Response[pushv1.PushResponse], error) {
// 	fmt.Printf("receiver received push for %#v\n", req)
// 	return connect.NewResponse(&pushv1.PushResponse{}), nil
// }

// Appender implements the pyroscope.Appendable interface.
func (r *receiver) Appender() pyroscope.Appender {
	return r
}

// Append implements the Appender interface.
func (r *receiver) Append(ctx context.Context, lbs labels.Labels, samples []*pyroscope.RawSample) error {
	batch, err := r.db.PrepareBatch(context.Background(), "INSERT INTO profiles_v3 (timestamp, profileType, serviceName, label_app_name, label_build_id, label_cgroup_path, label_colo_name, label_container_name, label_dpkg_version, label_env, label_exe, label_instance, sumOfValues, profile, stackMap.stack, stackMap.value)")
	if err != nil {
		return nil
	}

	defer batch.Close()

	for _, sample := range samples {
		profile, err := profile.ParseData(sample.RawProfile)
		if err != nil {
			return err
		}

		// if lbs.Get("container_name") == "/prometheus" {
		// 	fmt.Println(base64.StdEncoding.EncodeToString(sample.RawProfile))
		// }

		for sampleTypeIdx := range profile.SampleType {
			name := ""
			serviceName := ""

			sampleType := profile.SampleType[sampleTypeIdx].Type
			sampleUnit := profile.SampleType[sampleTypeIdx].Unit
			periodType := profile.PeriodType.Type
			periodUnit := profile.PeriodType.Unit

			final := map[string]string{}

			lbs.Range(func(label labels.Label) {
				switch label.Name {
				case "__name__":
					name = label.Value
				case "service_name":
					serviceName = label.Value
				default:
					if !strings.HasPrefix(label.Name, model.ReservedLabelPrefix) {
						final[label.Name] = label.Value
					}
				}
			})

			for name, value := range r.cfg.ExternalLabels {
				final[name] = value
			}

			sum := 0
			stacks := map[string]uint64{}

			for _, sample := range profile.Sample {
				sum += int(sample.Value[sampleTypeIdx])

				stack := []string{}
				for _, location := range sample.Location {
					file := "file:??"
					if location.Mapping != nil {
						file = location.Mapping.File
					}

					if len(location.Line) > 0 {
						for _, line := range location.Line {
							if line.Function == nil {
								stack = append(stack, fmt.Sprintf("%v [unknown]", file))
							} else {
								stack = append(stack, line.Function.Name)
							}
						}
					} else {
						stack = append(stack, fmt.Sprintf("%v [unknown]", file))
					}
				}

				slices.Reverse(stack)

				stacks[strings.Join(stack, " → ")] += uint64(sample.Value[sampleTypeIdx])
			}

			nestedStacks := [][]string{}
			nestedValues := []uint64{}
			for stack, value := range stacks {
				nestedStacks = append(nestedStacks, strings.Split(stack, " → "))
				nestedValues = append(nestedValues, value)
			}

			// serialized, err := json.Marshal(final)
			// if err != nil {
			// 	return err
			// }

			buf := bytes.NewBuffer(nil)
			err = profile.WriteUncompressed(buf)
			if err != nil {
				return err
			}

			// if lbs.Get("container_name") == "/prometheus" {
			// 	fmt.Println(base64.StdEncoding.EncodeToString(buf.Bytes()))
			// }

			err = batch.Append(
				time.Now(),
				fmt.Sprintf("%s:%s:%s:%s:%s", name, sampleType, sampleUnit, periodType, periodUnit),
				serviceName,
				final["container_name"],
				final["build_id"],
				final["cgroup_path"],
				final["colo_name"],
				final["container_name"],
				final["dpkg_version"],
				final["env"],
				final["exe"],
				final["instance"],
				sum,
				buf.Bytes(),
				nestedStacks,
				nestedValues,
			)
			if err != nil {
				return nil
			}
		}
	}

	err = batch.Send()
	if err != nil {
		return err
	}

	return nil
}

// AppendIngest implements the pyroscope.Appender interface.
func (r *receiver) AppendIngest(ctx context.Context, profile *pyroscope.IncomingProfile) error {
	// fmt.Printf("receiver received append ingest for %#v\n", profile)
	return nil
}
