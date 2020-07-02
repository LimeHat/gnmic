// Copyright © 2020 Karim Radhouani <medkarimrdi@gmail.com>
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/gnxi/utils/xpath"
	grpc_prometheus "github.com/grpc-ecosystem/go-grpc-prometheus"
	"github.com/karimra/gnmic/collector"
	"github.com/karimra/gnmic/outputs"
	_ "github.com/karimra/gnmic/outputs/all"
	"github.com/manifoldco/promptui"
	"github.com/mitchellh/mapstructure"
	"github.com/openconfig/gnmi/proto/gnmi"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/encoding/prototext"
)

type msg struct {
	Meta             map[string]interface{} `json:"meta,omitempty"`
	Source           string                 `json:"source,omitempty"`
	SystemName       string                 `json:"system-name,omitempty"`
	SubscriptionName string                 `json:"subscription-name,omitempty"`
	Timestamp        int64                  `json:"timestamp,omitempty"`
	Time             *time.Time             `json:"time,omitempty"`
	Prefix           string                 `json:"prefix,omitempty"`
	Updates          []*update              `json:"updates,omitempty"`
	Deletes          []string               `json:"deletes,omitempty"`
}
type update struct {
	Path   string
	Values map[string]interface{} `json:"values,omitempty"`
}

// subscribeCmd represents the subscribe command
var subscribeCmd = &cobra.Command{
	Use:     "subscribe",
	Aliases: []string{"sub"},
	Short:   "subscribe to gnmi updates on targets",

	RunE: func(cmd *cobra.Command, args []string) error {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		setupCloseHandler(cancel)
		debug := viper.GetBool("debug")
		targetsConfig, err := createTargets()
		if err != nil {
			return fmt.Errorf("failed getting targets config: %v", err)
		}
		if debug {
			logger.Printf("targets: %s", targetsConfig)
		}
		subscriptionsConfig, err := getSubscriptions()
		if err != nil {
			return fmt.Errorf("failed getting subscriptions config: %v", err)
		}
		if debug {
			logger.Printf("subscriptions: %s", subscriptionsConfig)
		}
		outs, err := getOutputs()
		if err != nil {
			return err
		}
		if debug {
			logger.Printf("outputs: %+v", outs)
		}
		defer func() {
			for _, outputs := range outs {
				for _, o := range outputs {
					o.Close()
				}
			}
		}()
		cfg := &collector.Config{
			PrometheusAddress: viper.GetString("prometheus-address"),
			Debug:             viper.GetBool("debug"),
		}

		coll := collector.NewCollector(ctx, cfg, targetsConfig, subscriptionsConfig, outs, createCollectorDialOpts(), logger)

		wg := new(sync.WaitGroup)
		wg.Add(len(targetsConfig))
		for tName := range coll.Targets {
			go func(tn string) {
				defer wg.Done()
				err = coll.Subscribe(tn)
				if err != nil {
					logger.Printf("failed subscribing to target '%s': %v", tn, err)
				}
			}(tName)
		}
		wg.Wait()
		polledTargetsSubscriptions := coll.PolledSubscriptionsTargets()
		if len(polledTargetsSubscriptions) > 0 {
			pollTargets := make([]string, 0, len(polledTargetsSubscriptions))
			for t := range polledTargetsSubscriptions {
				pollTargets = append(pollTargets, t)
			}
			sort.Slice(pollTargets, func(i, j int) bool {
				return pollTargets[i] < pollTargets[j]
			})
			s := promptui.Select{
				Label:        "select target to poll",
				Items:        pollTargets,
				HideSelected: true,
			}
			waitChan := make(chan struct{}, 1)
			waitChan <- struct{}{}
			go func() {
				for {
					select {
					case <-waitChan:
						_, name, err := s.Run()
						if err != nil {
							fmt.Printf("failed selecting target to poll: %v\n", err)
							continue
						}
						ss := promptui.Select{
							Label:        "select subscription to poll",
							Items:        polledTargetsSubscriptions[name],
							HideSelected: true,
						}
						_, subName, err := ss.Run()
						if err != nil {
							fmt.Printf("failed selecting subscription to poll: %v\n", err)
							continue
						}
						response, err := coll.TargetPoll(name, subName)
						if err != nil {
							fmt.Printf("target '%s', subscription '%s': poll response error:%v", name, subName, err)
							continue
						}
						b, err := coll.FormatMsg(nil, response)
						if err != nil {
							fmt.Printf("target '%s', subscription '%s': poll response formatting error:%v", name, subName, err)
							continue
						}
						dst := new(bytes.Buffer)
						err = json.Indent(dst, b, "", "  ")
						if err != nil {
							fmt.Printf("failed to indent poll response from '%s': %v\n", name, err)
							continue
						}
						fmt.Println(string(b))
						waitChan <- struct{}{}
					case <-ctx.Done():
						return
					}
				}
			}()
		}
		coll.Start()
		return nil
	},
}

func subRequest(ctx context.Context,
	req *gnmi.SubscribeRequest,
	target *collector.Target,
	wg *sync.WaitGroup,
	polledSubsChan map[string]chan string,
	waitChan chan struct{},
	clientMetrics *grpc_prometheus.ClientMetrics,
	outs []outputs.Output,
) {
	defer wg.Done()
	opts := createCollectorDialOpts()
	if clientMetrics != nil {
		opts = append(opts, grpc.WithStreamInterceptor(clientMetrics.StreamClientInterceptor()))
	}
	err := target.CreateGNMIClient(ctx, opts...)
	if err != nil {
		logger.Printf("failed to create a client for target '%s' : %v", target.Config.Name, err)
		return
	}
	xsubscReq := req
	models := viper.GetStringSlice("subscribe-model")
	if len(models) > 0 {
		spModels, unspModels, err := filterModels(ctx, target, models)
		if err != nil {
			logger.Printf("failed getting supported models from '%s': %v", target.Config.Address, err)
			return
		}
		if len(unspModels) > 0 {
			logger.Printf("found unsupported models for target '%s': %+v", target.Config.Address, unspModels)
		}
		if len(spModels) > 0 {
			modelsData := make([]*gnmi.ModelData, 0, len(spModels))
			for _, m := range spModels {
				modelsData = append(modelsData, m)
			}
			xsubscReq = &gnmi.SubscribeRequest{
				Request: &gnmi.SubscribeRequest_Subscribe{
					Subscribe: &gnmi.SubscriptionList{
						Prefix:       req.GetSubscribe().GetPrefix(),
						Mode:         req.GetSubscribe().GetMode(),
						Encoding:     req.GetSubscribe().GetEncoding(),
						Subscription: req.GetSubscribe().GetSubscription(),
						UseModels:    modelsData,
						Qos:          req.GetSubscribe().GetQos(),
						UpdatesOnly:  viper.GetBool("subscribe-updates-only"),
					},
				},
			}
		}
	}
	go target.Subscribe(ctx, xsubscReq, "")
	for {
		lock := new(sync.Mutex)
		select {
		case subscribeResponse := <-target.SubscribeResponses:
			switch resp := subscribeResponse.Response.Response.(type) {
			case *gnmi.SubscribeResponse_Update:
				b, err := formatSubscribeResponse(map[string]interface{}{"source": target.Config.Address}, subscribeResponse.Response)
				if err != nil {
					logger.Printf("failed to format subscribe response: %v", err)
					return
				}
				m := outputs.Meta{}
				m["source"] = target.Config.Address
				for _, o := range outs {
					go o.Write(b, m)
				}
				if !viper.GetBool("subscribe-quiet") && viper.GetString("format") != "textproto" {
					buff := new(bytes.Buffer)
					err = json.Indent(buff, b, "", "  ")
					if err != nil {
						logger.Printf("failed to indent msg: err=%v, msg=%s", err, string(b))
						return
					}
					lock.Lock()
					fmt.Println(buff.String())
					lock.Unlock()
				}
			case *gnmi.SubscribeResponse_SyncResponse:
				logger.Printf("received sync response=%+v from %s\n", resp.SyncResponse, target.Config.Address)
				if req.GetSubscribe().Mode == gnmi.SubscriptionList_ONCE {
					return
				}
			}
		case err := <-target.Errors:
			logger.Printf("subscription error: %v", err)
		case <-ctx.Done():
			return
		}
	}
}

func createSubscribeRequest() (*gnmi.SubscribeRequest, error) {
	paths := viper.GetStringSlice("subscribe-path")
	if len(paths) == 0 {
		return nil, errors.New("no path provided")
	}
	gnmiPrefix, err := xpath.ToGNMIPath(viper.GetString("subscribe-prefix"))
	if err != nil {
		return nil, fmt.Errorf("prefix parse error: %v", err)
	}
	encodingVal, ok := gnmi.Encoding_value[strings.Replace(strings.ToUpper(viper.GetString("encoding")), "-", "_", -1)]
	if !ok {
		return nil, fmt.Errorf("invalid encoding type '%s'", viper.GetString("encoding"))
	}
	modeVal, ok := gnmi.SubscriptionList_Mode_value[strings.ToUpper(viper.GetString("subscribe-subscription-mode"))]
	if !ok {
		return nil, fmt.Errorf("invalid subscription list type '%s'", viper.GetString("subscribe-subscription-mode"))
	}
	qos := &gnmi.QOSMarking{Marking: viper.GetUint32("qos")}
	samplingInterval, err := time.ParseDuration(viper.GetString("subscribe-sampling-interval"))
	if err != nil {
		return nil, err
	}
	heartbeatInterval, err := time.ParseDuration(viper.GetString("subscribe-heartbeat-interval"))
	if err != nil {
		return nil, err
	}
	subscriptions := make([]*gnmi.Subscription, len(paths))
	for i, p := range paths {
		gnmiPath, err := xpath.ToGNMIPath(strings.TrimSpace(p))
		if err != nil {
			return nil, fmt.Errorf("path parse error: %v", err)
		}
		subscriptions[i] = &gnmi.Subscription{Path: gnmiPath}
		switch gnmi.SubscriptionList_Mode(modeVal) {
		case gnmi.SubscriptionList_STREAM:
			mode, ok := gnmi.SubscriptionMode_value[strings.Replace(strings.ToUpper(viper.GetString("subscribe-stream-subscription-mode")), "-", "_", -1)]
			if !ok {
				return nil, fmt.Errorf("invalid streamed subscription mode %s", viper.GetString("subscribe-stream-subscription-mode"))
			}
			subscriptions[i].Mode = gnmi.SubscriptionMode(mode)
			switch gnmi.SubscriptionMode(mode) {
			case gnmi.SubscriptionMode_ON_CHANGE:
				subscriptions[i].HeartbeatInterval = uint64(heartbeatInterval.Nanoseconds())
			case gnmi.SubscriptionMode_SAMPLE:
				subscriptions[i].SampleInterval = uint64(samplingInterval.Nanoseconds())
				subscriptions[i].SuppressRedundant = viper.GetBool("subscribe-suppress-redundant")
				if subscriptions[i].SuppressRedundant {
					subscriptions[i].HeartbeatInterval = uint64(heartbeatInterval.Nanoseconds())
				}
			case gnmi.SubscriptionMode_TARGET_DEFINED:
				subscriptions[i].SampleInterval = uint64(samplingInterval.Nanoseconds())
				subscriptions[i].SuppressRedundant = viper.GetBool("subscribe-suppress-redundant")
				if subscriptions[i].SuppressRedundant {
					subscriptions[i].HeartbeatInterval = uint64(heartbeatInterval.Nanoseconds())
				}
			}
		}
	}
	return &gnmi.SubscribeRequest{
		Request: &gnmi.SubscribeRequest_Subscribe{
			Subscribe: &gnmi.SubscriptionList{
				Prefix:       gnmiPrefix,
				Mode:         gnmi.SubscriptionList_Mode(modeVal),
				Encoding:     gnmi.Encoding(encodingVal),
				Subscription: subscriptions,
				Qos:          qos,
				UpdatesOnly:  viper.GetBool("subscribe-updates-only"),
			},
		},
	}, nil
}

func init() {
	rootCmd.AddCommand(subscribeCmd)
	subscribeCmd.Flags().StringP("prefix", "", "", "subscribe request prefix")
	subscribeCmd.Flags().StringSliceP("path", "", []string{""}, "subscribe request paths")
	//subscribeCmd.MarkFlagRequired("path")
	subscribeCmd.Flags().Int32P("qos", "q", 20, "qos marking")
	subscribeCmd.Flags().BoolP("updates-only", "", false, "only updates to current state should be sent")
	subscribeCmd.Flags().StringP("subscription-mode", "", "stream", "one of: once, stream, poll")
	subscribeCmd.Flags().StringP("stream-subscription-mode", "", "target-defined", "one of: on-change, sample, target-defined")
	subscribeCmd.Flags().StringP("sampling-interval", "i", "10s",
		"sampling interval as a decimal number and a suffix unit, such as \"10s\" or \"1m30s\", minimum is 1s")
	subscribeCmd.Flags().BoolP("suppress-redundant", "", false, "suppress redundant update if the subscribed value didn't not change")
	subscribeCmd.Flags().StringP("heartbeat-interval", "", "0s", "heartbeat interval in case suppress-redundant is enabled")
	subscribeCmd.Flags().StringSliceP("model", "", []string{""}, "subscribe request used model(s)")
	subscribeCmd.Flags().BoolP("quiet", "", false, "suppress stdout printing")
	//
	viper.BindPFlag("subscribe-prefix", subscribeCmd.LocalFlags().Lookup("prefix"))
	viper.BindPFlag("subscribe-path", subscribeCmd.LocalFlags().Lookup("path"))
	viper.BindPFlag("subscribe-qos", subscribeCmd.LocalFlags().Lookup("qos"))
	viper.BindPFlag("subscribe-updates-only", subscribeCmd.LocalFlags().Lookup("updates-only"))
	viper.BindPFlag("subscribe-subscription-mode", subscribeCmd.LocalFlags().Lookup("subscription-mode"))
	viper.BindPFlag("subscribe-stream-subscription-mode", subscribeCmd.LocalFlags().Lookup("stream-subscription-mode"))
	viper.BindPFlag("subscribe-sampling-interval", subscribeCmd.LocalFlags().Lookup("sampling-interval"))
	viper.BindPFlag("subscribe-suppress-redundant", subscribeCmd.LocalFlags().Lookup("suppress-redundant"))
	viper.BindPFlag("subscribe-heartbeat-interval", subscribeCmd.LocalFlags().Lookup("heartbeat-interval"))
	viper.BindPFlag("subscribe-sub-model", subscribeCmd.LocalFlags().Lookup("model"))
	viper.BindPFlag("subscribe-quiet", subscribeCmd.LocalFlags().Lookup("quiet"))
}

func formatSubscribeResponse(meta map[string]interface{}, subResp *gnmi.SubscribeResponse) ([]byte, error) {
	switch resp := subResp.Response.(type) {
	case *gnmi.SubscribeResponse_Update:
		if viper.GetString("format") == "textproto" {
			return []byte(prototext.Format(subResp)), nil
		}
		msg := new(msg)
		msg.Timestamp = resp.Update.Timestamp
		t := time.Unix(0, resp.Update.Timestamp)
		msg.Time = &t
		if meta == nil {
			meta = make(map[string]interface{})
		}
		msg.Prefix = gnmiPathToXPath(resp.Update.Prefix)
		var ok bool
		if _, ok = meta["source"]; ok {
			msg.Source = fmt.Sprintf("%s", meta["source"])
		}
		if _, ok = meta["system-name"]; ok {
			msg.SystemName = fmt.Sprintf("%s", meta["system-name"])
		}
		if _, ok = meta["subscription-name"]; ok {
			msg.SubscriptionName = fmt.Sprintf("%s", meta["subscription-name"])
		}
		for i, upd := range resp.Update.Update {
			pathElems := make([]string, 0, len(upd.Path.Elem))
			for _, pElem := range upd.Path.Elem {
				pathElems = append(pathElems, pElem.GetName())
			}
			value, err := getValue(upd.Val)
			if err != nil {
				logger.Println(err)
			}
			msg.Updates = append(msg.Updates,
				&update{
					Path:   gnmiPathToXPath(upd.Path),
					Values: make(map[string]interface{}),
				})
			msg.Updates[i].Values[strings.Join(pathElems, "/")] = value
		}
		for _, del := range resp.Update.Delete {
			msg.Deletes = append(msg.Deletes, gnmiPathToXPath(del))
		}
		data, err := json.Marshal(msg)
		if err != nil {
			return nil, err
		}
		return data, nil
	}
	return nil, nil
}

func getOutputs() (map[string][]outputs.Output, error) {
	outDef := viper.GetStringMap("outputs")
	if outDef == nil {
		return nil, nil
	}
	logger.Printf("found outputs: %+v", outDef)
	outputDestinations := make(map[string][]outputs.Output, 0)
	for name, d := range outDef {
		dl := convert(d)
		switch outs := dl.(type) {
		case []interface{}:
			for _, ou := range outs {
				switch ou := ou.(type) {
				case map[string]interface{}:
					if outType, ok := ou["type"]; ok {
						if initalizer, ok := outputs.Outputs[outType.(string)]; ok {
							o := initalizer()
							err := o.Init(ou, logger)
							if err != nil {
								return nil, err
							}
							if outputDestinations[name] == nil {
								outputDestinations[name] = make([]outputs.Output, 0)
							}
							outputDestinations[name] = append(outputDestinations[name], o)
							continue
						}
						logger.Printf("unknown output type '%s'", outType)
						continue
					}
					logger.Printf("missing output 'type' under %v", ou)
				default:
					logger.Printf("unknown configuration format expecting a map[string]interface{}: %T : %v", d, d)
				}
			}
		default:
			logger.Printf("unknown configuration format: %T : %v", d, d)
			return nil, fmt.Errorf("unknown configuration format: %T : %v", d, d)
		}
	}
	if !viper.GetBool("quiet") {

	}
	return outputDestinations, nil
}

func getSubscriptions() (map[string]*collector.SubscriptionConfig, error) {
	subscriptions := make(map[string]*collector.SubscriptionConfig)
	paths := viper.GetStringSlice("subscribe-path")
	if len(paths) > 0 {
		sub := new(collector.SubscriptionConfig)
		sub.Name = "default"
		sub.Paths = paths
		sub.Prefix = viper.GetString("subscribe-prefix")
		sub.Mode = viper.GetString("subscribe-subscription-mode")
		sub.Encoding = viper.GetString("encoding")
		sub.Qos = viper.GetUint32("qos")
		sub.StreamMode = viper.GetString("subscribe-stream-subscription-mode")
		sub.HeartbeatInterval = viper.GetDuration("subscribe-heartbeat-interval")
		sub.SampleInterval = viper.GetDuration("subscribe-sampling-interval")
		sub.SuppressRedundant = viper.GetBool("subscribe-suppress-redundant")
		sub.UpdatesOnly = viper.GetBool("subscribe-updates-only")
		subscriptions["default"] = sub
		return subscriptions, nil
	}
	subDef := viper.GetStringMap("subscriptions")
	if subDef == nil || len(subDef) == 0 {
		return subscriptions, nil
	}
	var err error
	logger.Println(subDef)
	for sn, s := range subDef {
		sub := new(collector.SubscriptionConfig)
		decoder, err := mapstructure.NewDecoder(
			&mapstructure.DecoderConfig{
				DecodeHook: mapstructure.StringToTimeDurationHookFunc(),
				Result:     sub,
			})
		if err != nil {
			return nil, err
		}
		err = decoder.Decode(s)
		if err != nil {
			return nil, err
		}
		sub.Name = sn
		subscriptions[sn] = sub
	}
	return subscriptions, err
}
