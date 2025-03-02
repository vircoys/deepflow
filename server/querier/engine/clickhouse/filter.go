/*
 * Copyright (c) 2023 Yunshan Networks
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package clickhouse

import (
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"golang.org/x/exp/slices"

	"github.com/Knetic/govaluate"
	"github.com/deepflowio/deepflow/server/libs/utils"
	"github.com/deepflowio/deepflow/server/querier/common"
	"github.com/deepflowio/deepflow/server/querier/config"
	"github.com/deepflowio/deepflow/server/querier/engine/clickhouse/client"
	chCommon "github.com/deepflowio/deepflow/server/querier/engine/clickhouse/common"
	"github.com/deepflowio/deepflow/server/querier/engine/clickhouse/tag"
	"github.com/deepflowio/deepflow/server/querier/engine/clickhouse/view"
	"github.com/xwb1989/sqlparser"
	"inet.af/netaddr"
)

type Where struct {
	filter *view.Filters
	withs  []view.Node
	time   *view.Time
}

func (w *Where) Format(m *view.Model) {
	w.filter.Withs = w.withs
	if !w.filter.IsNull() {
		m.AddFilter(w.filter)
	}
}

type Having struct {
	Where
}

func (h *Having) Format(m *view.Model) {
	h.filter.Withs = h.withs
	if !h.filter.IsNull() {
		m.AddHaving(h.filter)
	}
}

func GetWhere(name, value string) WhereStatement {
	switch name {
	case "time":
		return &TimeTag{Value: value}
	default:
		return &WhereTag{Tag: name, Value: value}
	}
}

func TransWhereTagFunction(name string, args []string) (filter string) {
	funcName := strings.ToLower(name)
	switch funcName {
	case "exist":
		// Unsupported tags, filter is 1 = 1
		filter = "1=1"
		if len(args) != 1 {
			errorMessage := fmt.Sprintf("The parameters of function %s are not 1", funcName)
			log.Error(errorMessage)
			return
		}
		resource := strings.ToLower(strings.Trim(args[0], "`"))
		suffix := ""
		if strings.HasSuffix(resource, "_0") {
			suffix = "_0"
		} else if strings.HasSuffix(resource, "_1") {
			suffix = "_1"
		}
		resourceNoSuffix := strings.TrimSuffix(resource, "_0")
		resourceNoSuffix = strings.TrimSuffix(resourceNoSuffix, "_1")
		resourceNoID := strings.TrimSuffix(resourceNoSuffix, "_id")
		deviceTypeValue, ok := tag.DEVICE_MAP[resourceNoID]
		if ok {
			relatedOK := slices.Contains[string]([]string{"pod_service"}, resourceNoSuffix)
			if relatedOK {
				return
			}
			deviceTypeTagSuffix := "l3_device_type" + suffix
			filter = fmt.Sprintf("%s=%d", deviceTypeTagSuffix, deviceTypeValue)
			return
		} else if strings.HasPrefix(resourceNoSuffix, "k8s.label.") {
			podIDSuffix := "pod_id" + suffix
			serviceIDSuffix := "service_id" + suffix
			tagNoPreffix := strings.TrimPrefix(resourceNoSuffix, "k8s.label.")
			filter = fmt.Sprintf("((toUInt64(%s) IN (SELECT id FROM flow_tag.pod_service_k8s_label_map WHERE key='%s')) OR (toUInt64(%s) IN (SELECT id FROM flow_tag.pod_k8s_label_map WHERE key='%s')))", serviceIDSuffix, tagNoPreffix, podIDSuffix, tagNoPreffix)
		} else if strings.HasPrefix(resourceNoSuffix, "k8s.annotation.") {
			podIDSuffix := "pod_id" + suffix
			serviceIDSuffix := "service_id" + suffix
			tagNoPreffix := strings.TrimPrefix(resourceNoSuffix, "k8s.annotation.")
			filter = fmt.Sprintf("((toUInt64(%s) IN (SELECT id FROM flow_tag.pod_service_k8s_annotation_map WHERE key='%s')) OR (toUInt64(%s) IN (SELECT id FROM flow_tag.pod_k8s_annotation_map WHERE key='%s')))", serviceIDSuffix, tagNoPreffix, podIDSuffix, tagNoPreffix)
		} else if strings.HasPrefix(resourceNoSuffix, "k8s.env.") {
			podIDSuffix := "pod_id" + suffix
			tagNoPreffix := strings.TrimPrefix(resourceNoSuffix, "k8s.env.")
			filter = fmt.Sprintf("toUInt64(%s) IN (SELECT id FROM flow_tag.pod_k8s_env_map WHERE key='%s')", podIDSuffix, tagNoPreffix)
		} else if strings.HasPrefix(resourceNoSuffix, "cloud.tag.") {
			deviceIDSuffix := "l3_device_id" + suffix
			deviceTypeSuffix := "l3_device_type" + suffix
			podNSIDSuffix := "pod_ns_id" + suffix
			tagNoPreffix := strings.TrimPrefix(resourceNoSuffix, "cloud.tag.")
			filter = fmt.Sprintf("((toUInt64(%s) IN (SELECT id FROM flow_tag.chost_cloud_tag_map WHERE key='%s') AND %s=1) OR (toUInt64(%s) IN (SELECT id FROM flow_tag.pod_ns_cloud_tag_map WHERE key='%s')))", deviceIDSuffix, tagNoPreffix, deviceTypeSuffix, podNSIDSuffix, tagNoPreffix)
		} else if strings.HasPrefix(resourceNoSuffix, "os.app.") {
			processIDSuffix := "gprocess_id" + suffix
			tagNoPreffix := strings.TrimPrefix(resourceNoSuffix, "os.app.")
			filter = fmt.Sprintf("toUInt64(%s) IN (SELECT pid FROM flow_tag.os_app_tag_map WHERE key='%s')", processIDSuffix, tagNoPreffix)
		} else if deviceTypeValue, ok = tag.TAP_PORT_DEVICE_MAP[resourceNoSuffix]; ok {
			filter = fmt.Sprintf("(toUInt64(vtap_id),toUInt64(tap_port)) IN (SELECT vtap_id,tap_port FROM flow_tag.vtap_port_map WHERE tap_port!=0 AND device_type=%d)", deviceTypeValue)
		}
	}
	return
}

type WhereStatement interface {
	Trans(sqlparser.Expr, *Where, *CHEngine) (view.Node, error)
}

type WhereTag struct {
	Tag   string
	Value string
}

func (t *WhereTag) Trans(expr sqlparser.Expr, w *Where, e *CHEngine) (view.Node, error) {
	op := expr.(*sqlparser.ComparisonExpr).Operator
	asTagMap := e.AsTagMap
	db := e.DB
	table := e.Table
	isRemoteRead := false
	remoteRead := e.Context.Value("remote_read")
	if remoteRead != nil {
		isRemoteRead = remoteRead.(bool)
	}
	tagItem, ok := tag.GetTag(strings.Trim(t.Tag, "`"), db, table, "default")
	whereTag := t.Tag
	if strings.ToLower(op) == "like" || strings.ToLower(op) == "not like" {
		t.Value = strings.ReplaceAll(t.Value, "*", "%")
		if strings.ToLower(op) == "like" {
			op = "ilike"
		} else {
			op = "not ilike"
		}
	} else if strings.ToLower(op) == "regexp" || strings.ToLower(op) == "not regexp" {
		if strings.ToLower(op) == "regexp" {
			op = "match"
		} else {
			op = "not match"
		}
	}
	filter := ""
	if !ok {
		preAsTag, ok := asTagMap[t.Tag]
		if ok {
			whereTag = preAsTag
			tagItem, ok = tag.GetTag(strings.Trim(preAsTag, "`"), db, table, "default")
			if !ok {
				switch preAsTag {
				case "mac_0", "mac_1", "tunnel_tx_mac_0", "tunnel_tx_mac_1", "tunnel_rx_mac_0", "tunnel_rx_mac_1":
					macValue := strings.TrimLeft(t.Value, "(")
					macValue = strings.TrimRight(macValue, ")")
					macSlice := strings.Split(macValue, ",")
					macs := []string{}
					for _, valueStr := range macSlice {
						valueStr = strings.TrimSpace(valueStr)
						valueStr = strings.Trim(valueStr, "'")
						mac, err := net.ParseMAC(valueStr)
						if err != nil {
							return nil, err
						}
						valueUInt64 := utils.Mac2Uint64(mac)
						macs = append(macs, fmt.Sprintf("'%v'", valueUInt64))
					}
					if len(macs) != 0 {
						macsStr := strings.Join(macs, ",")
						if strings.ToLower(op) == "in" || strings.ToLower(op) == "not in" {
							macsStr = "(" + macsStr + ")"
						}
						filter = fmt.Sprintf("%s %s %s", t.Tag, op, macsStr)
					}
				case "tap_port":
					macValue := strings.TrimLeft(t.Value, "(")
					macValue = strings.TrimRight(macValue, ")")
					macSlice := strings.Split(macValue, ",")
					macs := []string{}
					for _, valueStr := range macSlice {
						valueStr = strings.TrimSpace(valueStr)
						valueStr = strings.Trim(valueStr, "'")
						ip := net.ParseIP(valueStr)
						if ip != nil {
							ip4 := ip.To4()
							if ip4 != nil {
								ipUint32 := utils.IpToUint32(ip4)
								macs = append(macs, fmt.Sprintf("'%v'", ipUint32))
							} else {
								return nil, errors.New(fmt.Sprintf("invalid ipv4 mac: %s", valueStr))
							}
						} else {
							macValueStr := "00:00:" + valueStr
							mac, err := net.ParseMAC(macValueStr)
							if err != nil {
								macs = append(macs, fmt.Sprintf("'%v'", valueStr))
							} else {
								valueUInt64 := utils.Mac2Uint64(mac)
								macs = append(macs, fmt.Sprintf("'%v'", valueUInt64))
							}
						}
					}
					if len(macs) != 0 {
						macsStr := strings.Join(macs, ",")
						if strings.ToLower(op) == "in" || strings.ToLower(op) == "not in" {
							macsStr = "(" + macsStr + ")"
						}
						filter = fmt.Sprintf("%s %s %s", t.Tag, op, macsStr)
					}
				default:
					preAsTag = strings.Trim(preAsTag, "`")
					if strings.HasPrefix(preAsTag, "k8s.label.") {
						if strings.HasSuffix(preAsTag, "_0") {
							tagItem, ok = tag.GetTag("k8s_label_0", db, table, "default")
						} else if strings.HasSuffix(preAsTag, "_1") {
							tagItem, ok = tag.GetTag("k8s_label_1", db, table, "default")
						} else {
							tagItem, ok = tag.GetTag("k8s_label", db, table, "default")
						}
						if ok {
							nameNoSuffix := strings.TrimSuffix(preAsTag, "_0")
							nameNoSuffix = strings.TrimSuffix(nameNoSuffix, "_1")
							nameNoPreffix := strings.TrimPrefix(nameNoSuffix, "k8s.label.")
							if strings.Contains(op, "match") {
								filter = fmt.Sprintf(tagItem.WhereRegexpTranslator, op, t.Value, nameNoPreffix, op, t.Value, nameNoPreffix)
							} else {
								filter = fmt.Sprintf(tagItem.WhereTranslator, op, t.Value, nameNoPreffix, op, t.Value, nameNoPreffix)
							}
							return &view.Expr{Value: filter}, nil
						}
					} else if strings.HasPrefix(preAsTag, "k8s.annotation.") {
						if strings.HasSuffix(preAsTag, "_0") {
							tagItem, ok = tag.GetTag("k8s_annotation_0", db, table, "default")
						} else if strings.HasSuffix(preAsTag, "_1") {
							tagItem, ok = tag.GetTag("k8s_annotation_1", db, table, "default")
						} else {
							tagItem, ok = tag.GetTag("k8s_annotation", db, table, "default")
						}
						if ok {
							nameNoSuffix := strings.TrimSuffix(preAsTag, "_0")
							nameNoSuffix = strings.TrimSuffix(nameNoSuffix, "_1")
							nameNoPreffix := strings.TrimPrefix(nameNoSuffix, "k8s.annotation.")
							if strings.Contains(op, "match") {
								filter = fmt.Sprintf(tagItem.WhereRegexpTranslator, op, t.Value, nameNoPreffix, op, t.Value, nameNoPreffix)
							} else {
								filter = fmt.Sprintf(tagItem.WhereTranslator, op, t.Value, nameNoPreffix, op, t.Value, nameNoPreffix)
							}
							return &view.Expr{Value: filter}, nil
						}
					} else if strings.HasPrefix(preAsTag, "k8s.env.") {
						if strings.HasSuffix(preAsTag, "_0") {
							tagItem, ok = tag.GetTag("k8s_env_0", db, table, "default")
						} else if strings.HasSuffix(preAsTag, "_1") {
							tagItem, ok = tag.GetTag("k8s_env_1", db, table, "default")
						} else {
							tagItem, ok = tag.GetTag("k8s_env", db, table, "default")
						}
						if ok {
							nameNoSuffix := strings.TrimSuffix(preAsTag, "_0")
							nameNoSuffix = strings.TrimSuffix(nameNoSuffix, "_1")
							nameNoPreffix := strings.TrimPrefix(nameNoSuffix, "k8s.env.")
							if strings.Contains(op, "match") {
								filter = fmt.Sprintf(tagItem.WhereRegexpTranslator, op, t.Value, nameNoPreffix)
							} else {
								filter = fmt.Sprintf(tagItem.WhereTranslator, op, t.Value, nameNoPreffix)
							}
							return &view.Expr{Value: filter}, nil
						}
					} else if strings.HasPrefix(preAsTag, "cloud.tag.") {
						if strings.HasSuffix(preAsTag, "_0") {
							tagItem, ok = tag.GetTag("cloud_tag_0", db, table, "default")
						} else if strings.HasSuffix(preAsTag, "_1") {
							tagItem, ok = tag.GetTag("cloud_tag_1", db, table, "default")
						} else {
							tagItem, ok = tag.GetTag("cloud_tag", db, table, "default")
						}
						if ok {
							nameNoSuffix := strings.TrimSuffix(preAsTag, "_0")
							nameNoSuffix = strings.TrimSuffix(nameNoSuffix, "_1")
							nameNoPreffix := strings.TrimPrefix(nameNoSuffix, "cloud.tag.")
							if strings.Contains(op, "match") {
								filter = fmt.Sprintf(tagItem.WhereRegexpTranslator, op, t.Value, nameNoPreffix, op, t.Value, nameNoPreffix)
							} else {
								filter = fmt.Sprintf(tagItem.WhereTranslator, op, t.Value, nameNoPreffix, op, t.Value, nameNoPreffix)
							}
							return &view.Expr{Value: filter}, nil
						}
					} else if strings.HasPrefix(preAsTag, "os.app.") {
						if strings.HasSuffix(preAsTag, "_0") {
							tagItem, ok = tag.GetTag("os_app_0", db, table, "default")
						} else if strings.HasSuffix(preAsTag, "_1") {
							tagItem, ok = tag.GetTag("os_app_1", db, table, "default")
						} else {
							tagItem, ok = tag.GetTag("os_app", db, table, "default")
						}
						if ok {
							nameNoSuffix := strings.TrimSuffix(preAsTag, "_0")
							nameNoSuffix = strings.TrimSuffix(nameNoSuffix, "_1")
							nameNoPreffix := strings.TrimPrefix(nameNoSuffix, "os.app.")
							if strings.Contains(op, "match") {
								filter = fmt.Sprintf(tagItem.WhereRegexpTranslator, op, t.Value, nameNoPreffix)
							} else {
								filter = fmt.Sprintf(tagItem.WhereTranslator, op, t.Value, nameNoPreffix)
							}
							return &view.Expr{Value: filter}, nil
						}
					} else if strings.HasPrefix(preAsTag, "tag.") || strings.HasPrefix(preAsTag, "attribute.") {
						if strings.HasPrefix(preAsTag, "tag.") {
							if isRemoteRead {
								originFilter := sqlparser.String(expr)
								filter, err := GetRemoteReadFilter(preAsTag, table, op, t.Value, originFilter, e)
								if err != nil {
									return nil, err
								}
								return &view.Expr{Value: filter}, nil
							}
							if db == chCommon.DB_NAME_PROMETHEUS {
								filter, err := GetPrometheusFilter(preAsTag, table, op, t.Value)
								if err != nil {
									return nil, err
								}
								return &view.Expr{Value: filter}, nil
							} else {
								tagItem, ok = tag.GetTag("tag.", db, table, "default")
							}
						} else {
							tagItem, ok = tag.GetTag("attribute.", db, table, "default")
						}
						if ok {
							nameNoPreffix := strings.TrimPrefix(preAsTag, "tag.")
							nameNoPreffix = strings.TrimPrefix(nameNoPreffix, "attribute.")
							if strings.Contains(op, "match") {
								filter = fmt.Sprintf(tagItem.WhereRegexpTranslator, op, nameNoPreffix, t.Value)
							} else {
								filter = fmt.Sprintf(tagItem.WhereTranslator, nameNoPreffix, op, t.Value)
							}
							return &view.Expr{Value: filter}, nil
						}
					}
					if strings.Contains(op, "match") {
						filter = fmt.Sprintf("%s(%s,%s)", op, t.Tag, t.Value)
					} else {
						filter = fmt.Sprintf("%s %s %s", t.Tag, op, t.Value)
					}
				}
				return &view.Expr{Value: filter}, nil
			}
		} else {
			switch t.Tag {
			case "mac_0", "mac_1", "tunnel_tx_mac_0", "tunnel_tx_mac_1", "tunnel_rx_mac_0", "tunnel_rx_mac_1":
				macValue := strings.TrimLeft(t.Value, "(")
				macValue = strings.TrimRight(macValue, ")")
				macSlice := strings.Split(macValue, ",")
				macs := []string{}
				for _, valueStr := range macSlice {
					valueStr = strings.TrimSpace(valueStr)
					valueStr = strings.Trim(valueStr, "'")
					mac, err := net.ParseMAC(valueStr)
					if err != nil {
						return nil, err
					}
					valueUInt64 := utils.Mac2Uint64(mac)
					macs = append(macs, fmt.Sprintf("'%v'", valueUInt64))
				}
				if len(macs) != 0 {
					macsStr := strings.Join(macs, ",")
					if strings.ToLower(op) == "in" || strings.ToLower(op) == "not in" {
						macsStr = "(" + macsStr + ")"
					}
					filter = fmt.Sprintf("%s %s %s", t.Tag, op, macsStr)
				}
			case "tap_port":
				macValue := strings.TrimLeft(t.Value, "(")
				macValue = strings.TrimRight(macValue, ")")
				macSlice := strings.Split(macValue, ",")
				macs := []string{}
				for _, valueStr := range macSlice {
					valueStr = strings.TrimSpace(valueStr)
					valueStr = strings.Trim(valueStr, "'")
					ip := net.ParseIP(valueStr)
					if ip != nil {
						ip4 := ip.To4()
						if ip4 != nil {
							ipUint32 := utils.IpToUint32(ip4)
							macs = append(macs, fmt.Sprintf("'%v'", ipUint32))
						} else {
							return nil, errors.New(fmt.Sprintf("invalid ipv4 mac: %s", valueStr))
						}
					} else {
						macValueStr := "00:00:" + valueStr
						mac, err := net.ParseMAC(macValueStr)
						if err != nil {
							macs = append(macs, fmt.Sprintf("'%v'", valueStr))
						} else {
							valueUInt64 := utils.Mac2Uint64(mac)
							macs = append(macs, fmt.Sprintf("'%v'", valueUInt64))
						}
					}
				}
				if len(macs) != 0 {
					macsStr := strings.Join(macs, ",")
					if strings.ToLower(op) == "in" || strings.ToLower(op) == "not in" {
						macsStr = "(" + macsStr + ")"
					}
					filter = fmt.Sprintf("%s %s %s", t.Tag, op, macsStr)
				}
			default:
				tagName := strings.Trim(t.Tag, "`")
				if strings.HasPrefix(tagName, "k8s.label.") {
					if strings.HasSuffix(tagName, "_0") {
						tagItem, ok = tag.GetTag("k8s_label_0", db, table, "default")
					} else if strings.HasSuffix(tagName, "_1") {
						tagItem, ok = tag.GetTag("k8s_label_1", db, table, "default")
					} else {
						tagItem, ok = tag.GetTag("k8s_label", db, table, "default")
					}
					if ok {
						nameNoSuffix := strings.TrimSuffix(tagName, "_0")
						nameNoSuffix = strings.TrimSuffix(nameNoSuffix, "_1")
						nameNoPreffix := strings.TrimPrefix(nameNoSuffix, "k8s.label.")
						if strings.Contains(op, "match") {
							filter = fmt.Sprintf(tagItem.WhereRegexpTranslator, op, t.Value, nameNoPreffix, op, t.Value, nameNoPreffix)
						} else {
							filter = fmt.Sprintf(tagItem.WhereTranslator, op, t.Value, nameNoPreffix, op, t.Value, nameNoPreffix)
						}
						return &view.Expr{Value: filter}, nil
					}
				} else if strings.HasPrefix(tagName, "k8s.annotation.") {
					if strings.HasSuffix(tagName, "_0") {
						tagItem, ok = tag.GetTag("k8s_annotation_0", db, table, "default")
					} else if strings.HasSuffix(tagName, "_1") {
						tagItem, ok = tag.GetTag("k8s_annotation_1", db, table, "default")
					} else {
						tagItem, ok = tag.GetTag("k8s_annotation", db, table, "default")
					}
					if ok {
						nameNoSuffix := strings.TrimSuffix(tagName, "_0")
						nameNoSuffix = strings.TrimSuffix(nameNoSuffix, "_1")
						nameNoPreffix := strings.TrimPrefix(nameNoSuffix, "k8s.annotation.")
						if strings.Contains(op, "match") {
							filter = fmt.Sprintf(tagItem.WhereRegexpTranslator, op, t.Value, nameNoPreffix, op, t.Value, nameNoPreffix)
						} else {
							filter = fmt.Sprintf(tagItem.WhereTranslator, op, t.Value, nameNoPreffix, op, t.Value, nameNoPreffix)
						}
						return &view.Expr{Value: filter}, nil
					}
				} else if strings.HasPrefix(tagName, "k8s.env.") {
					if strings.HasSuffix(tagName, "_0") {
						tagItem, ok = tag.GetTag("k8s_env_0", db, table, "default")
					} else if strings.HasSuffix(tagName, "_1") {
						tagItem, ok = tag.GetTag("k8s_env_1", db, table, "default")
					} else {
						tagItem, ok = tag.GetTag("k8s_env", db, table, "default")
					}
					if ok {
						nameNoSuffix := strings.TrimSuffix(tagName, "_0")
						nameNoSuffix = strings.TrimSuffix(nameNoSuffix, "_1")
						nameNoPreffix := strings.TrimPrefix(nameNoSuffix, "k8s.env.")
						if strings.Contains(op, "match") {
							filter = fmt.Sprintf(tagItem.WhereRegexpTranslator, op, t.Value, nameNoPreffix)
						} else {
							filter = fmt.Sprintf(tagItem.WhereTranslator, op, t.Value, nameNoPreffix)
						}
						return &view.Expr{Value: filter}, nil
					}
				} else if strings.HasPrefix(tagName, "cloud.tag.") {
					if strings.HasSuffix(tagName, "_0") {
						tagItem, ok = tag.GetTag("cloud_tag_0", db, table, "default")
					} else if strings.HasSuffix(tagName, "_1") {
						tagItem, ok = tag.GetTag("cloud_tag_1", db, table, "default")
					} else {
						tagItem, ok = tag.GetTag("cloud_tag", db, table, "default")
					}
					if ok {
						nameNoSuffix := strings.TrimSuffix(tagName, "_0")
						nameNoSuffix = strings.TrimSuffix(nameNoSuffix, "_1")
						nameNoPreffix := strings.TrimPrefix(nameNoSuffix, "cloud.tag.")
						if strings.Contains(op, "match") {
							filter = fmt.Sprintf(tagItem.WhereRegexpTranslator, op, t.Value, nameNoPreffix, op, t.Value, nameNoPreffix)
						} else {
							filter = fmt.Sprintf(tagItem.WhereTranslator, op, t.Value, nameNoPreffix, op, t.Value, nameNoPreffix)
						}
						return &view.Expr{Value: filter}, nil
					}
				} else if strings.HasPrefix(tagName, "os.app.") {
					if strings.HasSuffix(tagName, "_0") {
						tagItem, ok = tag.GetTag("os_app_0", db, table, "default")
					} else if strings.HasSuffix(tagName, "_1") {
						tagItem, ok = tag.GetTag("os_app_1", db, table, "default")
					} else {
						tagItem, ok = tag.GetTag("os_app", db, table, "default")
					}
					if ok {
						nameNoSuffix := strings.TrimSuffix(tagName, "_0")
						nameNoSuffix = strings.TrimSuffix(nameNoSuffix, "_1")
						nameNoPreffix := strings.TrimPrefix(nameNoSuffix, "os.app.")
						if strings.Contains(op, "match") {
							filter = fmt.Sprintf(tagItem.WhereRegexpTranslator, op, t.Value, nameNoPreffix)
						} else {
							filter = fmt.Sprintf(tagItem.WhereTranslator, op, t.Value, nameNoPreffix)
						}
						return &view.Expr{Value: filter}, nil
					}
				} else if strings.HasPrefix(tagName, "tag.") || strings.HasPrefix(tagName, "attribute.") {
					if strings.HasPrefix(tagName, "tag.") {
						if isRemoteRead {
							originFilter := sqlparser.String(expr)
							filter, err := GetRemoteReadFilter(tagName, table, op, t.Value, originFilter, e)
							if err != nil {
								return nil, err
							}
							return &view.Expr{Value: filter}, nil
						}
						if db == chCommon.DB_NAME_PROMETHEUS {
							filter, err := GetPrometheusFilter(tagName, table, op, t.Value)
							if err != nil {
								return nil, err
							}
							return &view.Expr{Value: filter}, nil
						} else {
							tagItem, ok = tag.GetTag("tag.", db, table, "default")
						}
					} else {
						tagItem, ok = tag.GetTag("attribute.", db, table, "default")
					}
					if ok {
						nameNoPreffix := strings.TrimPrefix(tagName, "tag.")
						nameNoPreffix = strings.TrimPrefix(nameNoPreffix, "attribute.")
						if strings.Contains(op, "match") {
							filter = fmt.Sprintf(tagItem.WhereRegexpTranslator, op, nameNoPreffix, t.Value)
						} else {
							filter = fmt.Sprintf(tagItem.WhereTranslator, nameNoPreffix, op, t.Value)
						}
						return &view.Expr{Value: filter}, nil
					}
				} else if strings.HasPrefix(tagName, "Enum(") {
					tagName = strings.TrimPrefix(tagName, "Enum(")
					tagName = strings.TrimSuffix(tagName, ")")
					tagItem, ok = tag.GetTag(tagName, db, table, "enum")
					if ok {
						if strings.Contains(op, "match") {
							filter = fmt.Sprintf(tagItem.WhereRegexpTranslator, op, t.Value)
						} else {
							filter = fmt.Sprintf(tagItem.WhereTranslator, op, t.Value)
						}
						return &view.Expr{Value: filter}, nil
					}
				}
				if strings.Contains(op, "match") {
					filter = fmt.Sprintf("%s(%s,%s)", op, t.Tag, t.Value)
				} else {
					filter = fmt.Sprintf("%s %s %s", t.Tag, op, t.Value)
				}
			}
			return &view.Expr{Value: filter}, nil
		}
	}
	whereFilter := tagItem.WhereTranslator
	if whereFilter != "" {
		switch whereTag {
		case "ip_version":
			versionValue := strings.TrimLeft(t.Value, "(")
			versionValue = strings.TrimRight(versionValue, ")")
			versionSlice := strings.Split(versionValue, ",")
			versions := []string{}
			for _, valueStr := range versionSlice {
				ipVersion := "0"
				if valueStr == "4" {
					ipVersion = "1"
				}
				versions = append(versions, ipVersion)
			}
			if len(versions) != 0 {
				versionsStr := strings.Join(versions, ",")
				if strings.ToLower(op) == "in" || strings.ToLower(op) == "not in" {
					versionsStr = "(" + versionsStr + ")"
				}
				whereFilter = fmt.Sprintf(tagItem.WhereTranslator, op, versionsStr)
			}
		case "is_internet", "is_internet_0", "is_internet_1":
			internetValue := strings.TrimLeft(t.Value, "(")
			internetValue = strings.TrimRight(internetValue, ")")
			internetSlice := strings.Split(internetValue, ",")
			hasTrue := false
			hasFalse := false
			newOP := ""
			for _, valueStr := range internetSlice {
				if valueStr == "1" || valueStr == "'1'" {
					hasTrue = true
				} else {
					hasFalse = true
				}
			}
			if hasTrue == true && hasFalse == true {
				whereFilter = "1=1"
			} else if hasTrue == true {
				if op == "=" || strings.ToLower(op) == "in" {
					newOP = "="
				} else {
					newOP = "!="
				}
			} else {
				if op == "=" || strings.ToLower(op) == "in" {
					newOP = "!="
				} else {
					newOP = "="
				}
			}
			whereFilter = fmt.Sprintf(tagItem.WhereTranslator, newOP)
		case "_id":
			idValue := strings.TrimLeft(t.Value, "(")
			idValue = strings.TrimRight(idValue, ")")
			idSlice := strings.Split(idValue, ",")
			whereFilters := []string{}
			for _, valueStr := range idSlice {
				valueStr = strings.Trim(t.Value, "'")
				valueInt, err := strconv.Atoi(valueStr)
				if err != nil {
					return nil, err
				}
				idFilter := fmt.Sprintf(tagItem.WhereTranslator, op, t.Value, valueInt)
				whereFilters = append(whereFilters, "("+idFilter+")")
			}
			if len(whereFilters) != 0 {
				equalFilter := "(" + strings.Join(whereFilters, " OR ") + ")"
				switch strings.ToLower(op) {
				case "not in":
					whereFilter = "not(" + equalFilter + ")"
				case "!=":
					whereFilter = "not(" + equalFilter + ")"
				default:
					whereFilter = equalFilter
				}
			}

		case "ip", "ip_0", "ip_1", "tunnel_tx_ip_0", "tunnel_tx_ip_1", "tunnel_rx_ip_0", "tunnel_rx_ip_1", "nat_real_ip", "nat_real_ip_0", "nat_real_ip_1":
			equalFilter := ""
			ipValues := strings.TrimLeft(t.Value, "(")
			ipValues = strings.TrimRight(ipValues, ")")
			ipSlice := strings.Split(ipValues, ",")
			ipOp := strings.ToLower(op)
			ipsFilter := ""
			cidrIPs := []string{}
			cidrFilters := []string{}
			ips := []string{}
			for _, ipValue := range ipSlice {
				ipValue = strings.Trim(ipValue, " ")
				if strings.Contains(ipValue, "/") {
					cidrIPs = append(cidrIPs, ipValue)
				} else {
					ips = append(ips, chCommon.IPFilterStringToHex(ipValue))
				}
			}
			for _, cidrIP := range cidrIPs {
				cidrIP = strings.Trim(cidrIP, "'")
				cidr, err := netaddr.ParseIPPrefix(cidrIP)
				if err != nil {
					return nil, err
				}
				minIP := chCommon.IPFilterStringToHex("'" + cidr.Masked().Range().From().String() + "'")
				maxIP := chCommon.IPFilterStringToHex("'" + cidr.Masked().Range().To().String() + "'")
				cidrFilter := ""
				if ipOp == ">=" || ipOp == ">" {
					cidrFilter = fmt.Sprintf(tagItem.WhereTranslator, ipOp, maxIP)
				} else if ipOp == "<=" || ipOp == "<" {
					cidrFilter = fmt.Sprintf(tagItem.WhereTranslator, ipOp, minIP)
				} else {
					cidrFilter = "(" + fmt.Sprintf(tagItem.WhereTranslator, ">=", minIP) + " AND " + fmt.Sprintf(tagItem.WhereTranslator, "<=", maxIP) + ")"
				}
				cidrFilters = append(cidrFilters, cidrFilter)
			}
			cidrFilterStr := ""
			if len(cidrFilters) != 0 {
				cidrFilterStr = "(" + strings.Join(cidrFilters, " OR ") + ")"
			}
			if len(ips) != 0 {
				if ipOp == ">=" || ipOp == "<=" || ipOp == ">" || ipOp == "<" {
					ipFilters := []string{}
					for _, ip := range ips {
						ipFilters = append(ipFilters, fmt.Sprintf(tagItem.WhereTranslator, ipOp, ip))
					}
					ipsFilter = "(" + strings.Join(ipFilters, " OR ") + ")"
				} else {
					ipsStr := strings.Join(ips, ",")
					equalOP := ""
					if ipOp == "in" || ipOp == "not in" {
						ipsStr = "(" + ipsStr + ")"
						equalOP = "in"
					} else {
						equalOP = "="
					}
					ipsFilter = "(" + fmt.Sprintf(tagItem.WhereTranslator, equalOP, ipsStr) + ")"
				}
			}
			finalFilters := []string{}
			if cidrFilterStr != "" {
				finalFilters = append(finalFilters, cidrFilterStr)
			}
			if ipsFilter != "" {
				finalFilters = append(finalFilters, ipsFilter)
			}
			equalFilter = "(" + strings.Join(finalFilters, " OR ") + ")"
			switch ipOp {
			case "not in":
				whereFilter = "not(" + equalFilter + ")"
			case "!=":
				whereFilter = "not(" + equalFilter + ")"
			default:
				whereFilter = equalFilter
			}
		case "pod_service_id", "pod_service_id_0", "pod_service_id_1", "natgw_id", "natgw_id_0", "natgw_id_1", "natgw", "natgw_0", "natgw_1",
			"lb_id", "lb_id_0", "lb_id_1", "lb", "lb_0", "lb_1", "lb_listener_id", "lb_listener_id_0", "lb_listener_id_1", "lb_listener", "lb_listener_0", "lb_listener_1":
			switch strings.ToLower(op) {
			case "not match":
				whereFilter = "not(" + fmt.Sprintf(tagItem.WhereRegexpTranslator, "match", t.Value) + ")"
			case "not ilike":
				whereFilter = "not(" + fmt.Sprintf(tagItem.WhereTranslator, "ilike", t.Value) + ")"
			case "not in":
				whereFilter = "not(" + fmt.Sprintf(tagItem.WhereTranslator, "in", t.Value) + ")"
			case "!=":
				whereFilter = "not(" + fmt.Sprintf(tagItem.WhereTranslator, "=", t.Value) + ")"
			case "match":
				whereFilter = fmt.Sprintf(tagItem.WhereRegexpTranslator, op, t.Value)
			default:
				whereFilter = fmt.Sprintf(tagItem.WhereTranslator, op, t.Value)
			}
		case "pod_ingress_id", "pod_ingress_id_0", "pod_ingress_id_1", "pod_ingress", "pod_ingress_0", "pod_ingress_1",
			"pod_service", "pod_service_0", "pod_service_1":
			switch strings.ToLower(op) {
			case "not match":
				whereFilter = "not(" + fmt.Sprintf(tagItem.WhereRegexpTranslator, "match", t.Value, "match", t.Value) + ")"
			case "not ilike":
				whereFilter = "not(" + fmt.Sprintf(tagItem.WhereTranslator, "ilike", t.Value, "ilike", t.Value) + ")"
			case "not in":
				whereFilter = "not(" + fmt.Sprintf(tagItem.WhereTranslator, "in", t.Value, "in", t.Value) + ")"
			case "!=":
				whereFilter = "not(" + fmt.Sprintf(tagItem.WhereTranslator, "=", t.Value, "=", t.Value) + ")"
			case "match":
				whereFilter = fmt.Sprintf(tagItem.WhereRegexpTranslator, op, t.Value, op, t.Value)
			default:
				whereFilter = fmt.Sprintf(tagItem.WhereTranslator, op, t.Value, op, t.Value)
			}
		case "resource_gl0", "resource_gl0_0", "resource_gl0_1", "resource_gl1", "resource_gl1_0", "resource_gl1_1",
			"resource_gl2", "resource_gl2_0", "resource_gl2_1", "resource_gl0_id", "resource_gl0_id_0", "resource_gl0_id_1",
			"resource_gl1_id", "resource_gl1_id_0", "resource_gl1_id_1", "resource_gl2_id", "resource_gl2_id_0", "resource_gl2_id_1",
			"auto_instance", "auto_instance_0", "auto_instance_1", "auto_instance_id", "auto_instance_id_0", "auto_instance_id_1",
			"auto_service", "auto_service_0", "auto_service_1", "auto_service_id", "auto_service_id_0", "auto_service_id_1":
			if strings.Contains(op, "match") {
				whereFilter = fmt.Sprintf(tagItem.WhereRegexpTranslator, op, t.Value, op, t.Value)
			} else {
				whereFilter = fmt.Sprintf(tagItem.WhereTranslator, op, t.Value, op, t.Value)
			}
		case "acl_gids":
			whereFilter = fmt.Sprintf(tagItem.WhereTranslator, t.Value)
		default:
			if strings.Contains(op, "match") {
				whereFilter = fmt.Sprintf(tagItem.WhereRegexpTranslator, op, t.Value)
			} else {
				whereFilter = fmt.Sprintf(tagItem.WhereTranslator, op, t.Value)
			}
		}
	} else {
		if strings.Contains(op, "match") {
			filter = fmt.Sprintf("%s(%s,%s)", op, t.Tag, t.Value)
		} else {
			filter = fmt.Sprintf("%s %s %s", t.Tag, op, t.Value)
		}
		return &view.Expr{Value: filter}, nil
	}
	return &view.Expr{Value: "(" + whereFilter + ")"}, nil
}

func GetPrometheusFilter(promTag, table, op, value string) (string, error) {
	filter := ""
	nameNoPreffix := strings.TrimPrefix(promTag, "tag.")
	metricID, ok := Prometheus.MetricNameToID[table]
	if !ok {
		errorMessage := fmt.Sprintf("%s not found", table)
		return filter, errors.New(errorMessage)
	}
	labelNameID, ok := Prometheus.LabelNameToID[nameNoPreffix]
	if !ok {
		errorMessage := fmt.Sprintf("%s not found", nameNoPreffix)
		return filter, errors.New(errorMessage)
	}
	// Determine whether the tag is app_label or target_label
	isAppLabel := false
	if appLabels, ok := Prometheus.MetricAppLabelLayout[table]; ok {
		for _, appLabel := range appLabels {
			if appLabel.AppLabelName == nameNoPreffix {
				isAppLabel = true
				if strings.Contains(op, "match") {
					filter = fmt.Sprintf("toUInt64(app_label_value_id_%d) IN (SELECT label_value_id FROM flow_tag.app_label_live_view WHERE label_name_id=%d and %s(label_value,%s))", appLabel.appLabelColumnIndex, labelNameID, op, value)
				} else {
					filter = fmt.Sprintf("toUInt64(app_label_value_id_%d) IN (SELECT label_value_id FROM flow_tag.app_label_live_view WHERE label_name_id=%d and label_value %s %s)", appLabel.appLabelColumnIndex, labelNameID, op, value)
				}
				break
			}
		}
	}
	if !isAppLabel {
		if strings.Contains(op, "match") {
			filter = fmt.Sprintf("toUInt64(target_id) IN (SELECT target_id FROM flow_tag.target_label_live_view WHERE metric_id=%d and label_name_id=%d and %s(label_value,%s))", metricID, labelNameID, op, value)
		} else {
			filter = fmt.Sprintf("toUInt64(target_id) IN (SELECT target_id FROM flow_tag.target_label_live_view WHERE metric_id=%d and label_name_id=%d and label_value %s %s)", metricID, labelNameID, op, value)
		}
	}
	return filter, nil
}

func GetRemoteReadFilter(promTag, table, op, value, originFilter string, e *CHEngine) (string, error) {
	filter := ""
	sql := ""
	isAppLabel := false
	nameNoPreffix := strings.TrimPrefix(promTag, "tag.")
	metricID, ok := Prometheus.MetricNameToID[table]
	if !ok {
		errorMessage := fmt.Sprintf("%s not found", table)
		return filter, errors.New(errorMessage)
	}
	labelNameID, ok := Prometheus.LabelNameToID[nameNoPreffix]
	if !ok {
		errorMessage := fmt.Sprintf("%s not found", nameNoPreffix)
		return filter, errors.New(errorMessage)
	}
	prometheusSubqueryCache := GetPrometheusSubqueryCache()
	// Determine whether the tag is app_label or target_label
	if appLabels, ok := Prometheus.MetricAppLabelLayout[table]; ok {
		for _, appLabel := range appLabels {
			if appLabel.AppLabelName == nameNoPreffix {
				isAppLabel = true
				cacheFilter, ok := prometheusSubqueryCache.PrometheusSubqueryCache.Get(originFilter)
				if ok {
					filter = cacheFilter.Filter
					timeout := cacheFilter.Time
					if time.Since(timeout) < time.Duration(config.Cfg.PrometheusIdSubqueryLruTimeout) {
						return filter, nil
					}
				}

				// lru timeout
				if strings.Contains(op, "match") {
					sql = fmt.Sprintf("SELECT label_value_id FROM flow_tag.app_label_live_view WHERE label_name_id=%d and %s(label_value,%s) GROUP BY label_value_id", labelNameID, op, value)
				} else {
					sql = fmt.Sprintf("SELECT label_value_id FROM flow_tag.app_label_live_view WHERE label_name_id=%d and label_value %s %s GROUP BY label_value_id", labelNameID, op, value)
				}
				chClient := client.Client{
					Host:     config.Cfg.Clickhouse.Host,
					Port:     config.Cfg.Clickhouse.Port,
					UserName: config.Cfg.Clickhouse.User,
					Password: config.Cfg.Clickhouse.Password,
					DB:       "flow_tag",
				}
				appLabelRst, err := chClient.DoQuery(&client.QueryParams{Sql: sql})
				if err != nil {
					return "", err
				}
				valueIDs := []string{}
				for _, v := range appLabelRst.Values {
					valueID := v.([]interface{})[0]
					valueIDInt := valueID.(int)
					valueIDString := fmt.Sprintf("%d", valueIDInt)
					valueIDs = append(valueIDs, valueIDString)
				}
				valueIDFilter := strings.Join(valueIDs, ",")
				if valueIDFilter == "" {
					filter = "1!=1"
				} else {
					filter = fmt.Sprintf("app_label_value_id_%d IN (%s)", appLabel.appLabelColumnIndex, valueIDFilter)
				}
				entryValue := common.EntryValue{Time: time.Now(), Filter: filter}
				prometheusSubqueryCache.PrometheusSubqueryCache.Add(originFilter, entryValue)
				return filter, nil
			}
		}
	}
	if !isAppLabel {
		transFilter := ""
		if strings.Contains(op, "match") {
			transFilter = fmt.Sprintf("SELECT target_id FROM flow_tag.target_label_live_view WHERE metric_id=%d and label_name_id=%d and %s(label_value,%s) GROUP BY target_id", metricID, labelNameID, op, value)
		} else {
			transFilter = fmt.Sprintf("SELECT target_id FROM flow_tag.target_label_live_view WHERE metric_id=%d and label_name_id=%d and label_value %s %s GROUP BY target_id", metricID, labelNameID, op, value)
		}
		targetLabelFilter := TargetLabelFilter{OriginFilter: originFilter, TransFilter: transFilter}
		e.TargetLabelFilters = append(e.TargetLabelFilters, targetLabelFilter)
		// FIXME delete placeholders
		filter = ""
	}
	return filter, nil
}

type TimeTag struct {
	Value string
}

func (t *TimeTag) Trans(expr sqlparser.Expr, w *Where, e *CHEngine) (view.Node, error) {
	compareExpr := expr.(*sqlparser.ComparisonExpr)
	time, err := strconv.ParseInt(t.Value, 10, 64)
	if err == nil {
	} else {
		timeExpr, err := govaluate.NewEvaluableExpression(t.Value)
		if err != nil {
			return nil, err
		}
		timeValue, err := timeExpr.Evaluate(nil)
		if err != nil {
			return nil, err
		}
		time = int64(timeValue.(float64))
	}
	if compareExpr.Operator == ">=" {
		w.time.AddTimeStart(time)
	} else if compareExpr.Operator == "<=" {
		w.time.AddTimeEnd(time)
	}
	return &view.Expr{Value: sqlparser.String(compareExpr)}, nil
}

type WhereFunction struct {
	Function view.Node
	Value    string
}

func (f *WhereFunction) Trans(expr sqlparser.Expr, w *Where, asTagMap map[string]string, db, table string) (view.Node, error) {
	opName := expr.(*sqlparser.ComparisonExpr).Operator
	op, opType := view.GetOperator(expr.(*sqlparser.ComparisonExpr).Operator)
	right := view.Expr{Value: ""}
	if opType == view.OPERATOER_UNKNOWN {
		return nil, errors.New(fmt.Sprintf("opeartor: %s not support", expr.(*sqlparser.ComparisonExpr).Operator))
	}
	function := strings.Trim(f.Function.ToString(), "`")
	if strings.HasPrefix(function, "Enum(") {
		var isIntEnum = true
		tagName := strings.TrimPrefix(function, "Enum(")
		tagName = strings.TrimSuffix(tagName, ")")
		tagName = strings.Trim(tagName, "`")
		tagEnum := strings.TrimSuffix(tagName, "_0")
		tagEnum = strings.TrimSuffix(tagEnum, "_1")
		tagDescription, ok := tag.TAG_DESCRIPTIONS[tag.TagDescriptionKey{
			DB: db, Table: table, TagName: tagEnum,
		}]
		if !ok {
			return nil, errors.New(fmt.Sprintf("no tag %s in %s.%s", tagName, db, table))
		}
		_, isStringEnumOK := tag.TAG_STRING_ENUMS[tagDescription.EnumFile]
		if isStringEnumOK {
			isIntEnum = false
		}
		tagItem, ok := tag.GetTag(tagName, db, table, "enum")
		if !ok {
			right = view.Expr{Value: f.Value}
		} else {
			whereFilter := tagItem.WhereTranslator
			if strings.ToLower(opName) == "like" || strings.ToLower(opName) == "not like" {
				f.Value = strings.ReplaceAll(f.Value, "*", "%")
				if strings.ToLower(opName) == "like" {
					opName = "ilike"
				} else {
					opName = "not ilike"
				}
			} else if strings.ToLower(opName) == "regexp" || strings.ToLower(opName) == "not regexp" {
				if strings.ToLower(opName) == "regexp" {
					opName = "match"
				} else {
					opName = "not match"
				}
			}
			enumFileName := strings.TrimSuffix(tagDescription.EnumFile, "."+config.Cfg.Language)
			switch strings.ToLower(expr.(*sqlparser.ComparisonExpr).Operator) {
			case "=":
				//when enum function operator is '=' , add 'or tag = xxx'
				if isIntEnum {
					intValue, err := strconv.Atoi(strings.Trim(f.Value, "'"))
					if err == nil {
						// when value type is int, add toUInt64() function
						whereFilter = fmt.Sprintf(tagItem.WhereTranslator, "=", f.Value, enumFileName) + " OR " + tagName + " = " + "toUInt64(" + strconv.Itoa(intValue) + ")"
					} else {
						whereFilter = fmt.Sprintf(tagItem.WhereTranslator, "=", f.Value, enumFileName)
					}
				} else {
					whereFilter = fmt.Sprintf(tagItem.WhereTranslator, "=", f.Value, enumFileName) + " OR " + tagName + " = " + f.Value
				}
			case "!=":
				//when enum function operator is '!=', add 'and tag != xxx'
				if isIntEnum {
					intValue, err := strconv.Atoi(strings.Trim(f.Value, "'"))
					if err == nil {
						// when value type is int, add toUInt64() function
						whereFilter = fmt.Sprintf(tagItem.WhereTranslator, opName, f.Value, enumFileName) + " AND " + tagName + " != " + "toUInt64(" + strconv.Itoa(intValue) + ")"
					} else {
						whereFilter = fmt.Sprintf(tagItem.WhereTranslator, opName, f.Value, enumFileName)
					}
				} else {
					whereFilter = fmt.Sprintf(tagItem.WhereTranslator, "=", f.Value, enumFileName) + " AND " + tagName + " != " + f.Value
				}
			default:
				if strings.Contains(opName, "match") {
					whereFilter = fmt.Sprintf(tagItem.WhereRegexpTranslator, opName, f.Value, enumFileName)
				} else {
					whereFilter = fmt.Sprintf(tagItem.WhereTranslator, opName, f.Value, enumFileName)
				}
			}
			return &view.Expr{Value: "(" + whereFilter + ")"}, nil
		}
	} else {
		right = view.Expr{Value: f.Value}
	}
	w.withs = append(w.withs, f.Function.GetWiths()...)
	return &view.BinaryExpr{Left: f.Function, Right: &right, Op: op}, nil
}
