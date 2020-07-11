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
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/google/gnxi/utils/xpath"
	"github.com/karimra/gnmic/collector"
	"github.com/openconfig/gnmi/proto/gnmi"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"google.golang.org/protobuf/encoding/prototext"
)

// getCmd represents the get command
var getCmd = &cobra.Command{
	Use:   "get",
	Short: "run gnmi get on targets",

	RunE: func(cmd *cobra.Command, args []string) error {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		setupCloseHandler(cancel)
		targets, err := createTargets()
		if err != nil {
			return err
		}
		req, err := createGetRequest()
		if err != nil {
			return err
		}
		wg := new(sync.WaitGroup)
		wg.Add(len(targets))
		lock := new(sync.Mutex)
		for _, tc := range targets {
			go getRequest(ctx, req, collector.NewTarget(tc), wg, lock)
		}
		wg.Wait()
		return nil
	},
}

func getRequest(ctx context.Context, req *gnmi.GetRequest, target *collector.Target, wg *sync.WaitGroup, lock *sync.Mutex) {
	defer wg.Done()
	opts := createCollectorDialOpts()
	if err := target.CreateGNMIClient(ctx, opts...); err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			logger.Printf("failed to create a gRPC client for target '%s', timeout (%s) reached", target.Config.Name, target.Config.Timeout)
			return
		}
		logger.Printf("failed to create a client for target '%s' : %v", target.Config.Name, err)
		return
	}
	xreq := req
	models := viper.GetStringSlice("get-model")
	if len(models) > 0 {
		spModels, unspModels, err := filterModels(ctx, target, models)
		if err != nil {
			logger.Printf("failed getting supported models from '%s': %v", target.Config.Address, err)
			return
		}
		if len(unspModels) > 0 {
			logger.Printf("found unsupported models for target '%s': %+v", target.Config.Address, unspModels)
		}
		for _, m := range spModels {
			xreq.UseModels = append(xreq.UseModels, m)
		}
	}
	logger.Printf("sending gNMI GetRequest: prefix='%v', path='%v', type='%v', encoding='%v', models='%+v', extension='%+v' to %s",
		xreq.Prefix, xreq.Path, xreq.Type, xreq.Encoding, xreq.UseModels, xreq.Extension, target.Config.Address)
	response, err := target.Get(ctx, xreq)
	if err != nil {
		logger.Printf("failed sending GetRequest to %s: %v", target.Config.Address, err)
		return
	}
	lock.Lock()
	printGetResponse(target.Config.Name, response)
	lock.Unlock()
}

func printGetResponse(address string, response *gnmi.GetResponse) {
	printPrefix := ""
	//	addresses := viper.GetStringSlice("address")
	if numTargets() > 1 && !viper.GetBool("no-prefix") {
		printPrefix = fmt.Sprintf("[%s] ", address)
	}
	if viper.GetString("format") == "textproto" {
		fmt.Printf("%s\n", indent(printPrefix, prototext.Format(response)))
		return
	}
	for _, notif := range response.Notification {
		msg := new(msg)
		msg.Source = address
		msg.Timestamp = notif.Timestamp
		t := time.Unix(0, notif.Timestamp)
		msg.Time = &t
		msg.Prefix = gnmiPathToXPath(notif.Prefix)
		for i, upd := range notif.Update {
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
		dMsg, err := json.MarshalIndent(msg, printPrefix, "  ")
		if err != nil {
			logger.Printf("error marshling json msg:%v", err)
			return
		}
		fmt.Printf("%s%s\n", printPrefix, string(dMsg))
	}
	fmt.Println()
}

func init() {
	rootCmd.AddCommand(getCmd)

	getCmd.Flags().StringSliceP("path", "", []string{""}, "get request paths")
	getCmd.MarkFlagRequired("path")
	getCmd.Flags().StringP("prefix", "", "", "get request prefix")
	getCmd.Flags().StringSliceP("model", "", []string{""}, "get request model(s)")
	getCmd.Flags().StringP("type", "t", "ALL", "the type of data that is requested from the target. one of: ALL, CONFIG, STATE, OPERATIONAL")
	getCmd.Flags().StringP("target", "", "", "get request target")
	viper.BindPFlag("get-path", getCmd.LocalFlags().Lookup("path"))
	viper.BindPFlag("get-prefix", getCmd.LocalFlags().Lookup("prefix"))
	viper.BindPFlag("get-model", getCmd.LocalFlags().Lookup("model"))
	viper.BindPFlag("get-type", getCmd.LocalFlags().Lookup("type"))
	viper.BindPFlag("get-target", getCmd.LocalFlags().Lookup("target"))
}

func createGetRequest() (*gnmi.GetRequest, error) {
	encodingVal, ok := gnmi.Encoding_value[strings.Replace(strings.ToUpper(viper.GetString("encoding")), "-", "_", -1)]
	if !ok {
		return nil, fmt.Errorf("invalid encoding type '%s'", viper.GetString("encoding"))
	}
	paths := viper.GetStringSlice("get-path")
	req := &gnmi.GetRequest{
		UseModels: make([]*gnmi.ModelData, 0),
		Path:      make([]*gnmi.Path, 0, len(paths)),
		Encoding:  gnmi.Encoding(encodingVal),
	}
	prefix := viper.GetString("get-prefix")
	target := viper.GetString("get-target")
	if prefix != "" || target != "" {
		gnmiPrefix, err := xpath.ToGNMIPath(prefix)
		if err != nil {
			return nil, fmt.Errorf("prefix parse error: %v", err)
		}
		if gnmiPrefix == nil && target != "" {
			gnmiPrefix = new(gnmi.Path)
			gnmiPrefix.Target = target
		}
		req.Prefix = gnmiPrefix
	}

	dataType := viper.GetString("get-type")
	if dataType != "" {
		dti, ok := gnmi.GetRequest_DataType_value[strings.ToUpper(dataType)]
		if !ok {
			return nil, fmt.Errorf("unknown data type %s", dataType)
		}
		req.Type = gnmi.GetRequest_DataType(dti)
	}
	for _, p := range paths {
		gnmiPath, err := xpath.ToGNMIPath(strings.TrimSpace(p))
		if err != nil {
			return nil, fmt.Errorf("path parse error: %v", err)
		}
		req.Path = append(req.Path, gnmiPath)
	}
	return req, nil
}
