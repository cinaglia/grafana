package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/grafana/grafana/pkg/api/dtos"
	"github.com/grafana/grafana/pkg/bus"
	"github.com/grafana/grafana/pkg/components/simplejson"
	"github.com/grafana/grafana/pkg/metrics"
	m "github.com/grafana/grafana/pkg/models"
	"github.com/grafana/grafana/pkg/services/guardian"
	"github.com/grafana/grafana/pkg/setting"
	"github.com/grafana/grafana/pkg/util"
)

func GetSharingOptions(c *m.ReqContext) {
	c.JSON(200, util.DynMap{
		"externalSnapshotURL":  setting.ExternalSnapshotUrl,
		"externalSnapshotName": setting.ExternalSnapshotName,
		"externalEnabled":      setting.ExternalEnabled,
	})
}

type CreateExternalSnapshotResponse struct {
	Key       string
	DeleteKey string
	Url       string
	DeleteUrl string
}

func createExternalDashboardSnapshot(cmd m.CreateDashboardSnapshotCommand) (*CreateExternalSnapshotResponse, error) {
	var createSnapshotResponse CreateExternalSnapshotResponse
	message := map[string]interface{}{
		"name":      cmd.Name,
		"expires":   cmd.Expires,
		"dashboard": cmd.Dashboard,
	}

	messageBytes, err := simplejson.NewFromAny(message).Encode()
	if err != nil {
		return nil, err
	}

	response, err := http.Post(setting.ExternalSnapshotUrl+"/api/snapshots", "application/json", bytes.NewBuffer(messageBytes))
	if response != nil {
		defer response.Body.Close()
	}

	if err != nil {
		return nil, err
	}

	if response.StatusCode != 200 {
		return nil, fmt.Errorf("Create external snapshot response status code %d", response.StatusCode)
	}

	if err := json.NewDecoder(response.Body).Decode(&createSnapshotResponse); err != nil {
		return nil, err
	}

	return &createSnapshotResponse, nil
}

// POST /api/snapshots
func CreateDashboardSnapshot(c *m.ReqContext, cmd m.CreateDashboardSnapshotCommand) {
	if cmd.Name == "" {
		cmd.Name = "Unnamed snapshot"
	}

	url := setting.ToAbsUrl("dashboard/snapshot/" + cmd.Key)
	cmd.ExternalUrl = ""
	cmd.OrgId = c.OrgId
	cmd.UserId = c.UserId

	if cmd.External {
		if !setting.ExternalEnabled {
			c.JsonApiErr(403, "External dashboard creation is disabled", nil)
			return
		}

		response, err := createExternalDashboardSnapshot(cmd)
		if err != nil {
			c.JsonApiErr(500, "Failed to create external snaphost", err)
		}

		url = response.Url
		cmd.Key = response.Key
		cmd.DeleteKey = response.DeleteKey
		cmd.ExternalUrl = response.Url
		cmd.Dashboard = simplejson.New()

		metrics.M_Api_Dashboard_Snapshot_External.Inc()
	} else {
		cmd.Key = util.GetRandomString(32)
		cmd.DeleteKey = util.GetRandomString(32)

		metrics.M_Api_Dashboard_Snapshot_Create.Inc()
	}

	if err := bus.Dispatch(&cmd); err != nil {
		c.JsonApiErr(500, "Failed to create snaphost", err)
		return
	}

	c.JSON(200, util.DynMap{
		"key":       cmd.Key,
		"deleteKey": cmd.DeleteKey,
		"url":       url,
		"deleteUrl": setting.ToAbsUrl("api/snapshots/" + cmd.Key),
	})
}

// GET /api/snapshots/:key
func GetDashboardSnapshot(c *m.ReqContext) {
	key := c.Params(":key")
	query := &m.GetDashboardSnapshotQuery{Key: key}

	err := bus.Dispatch(query)
	if err != nil {
		c.JsonApiErr(500, "Failed to get dashboard snapshot", err)
		return
	}

	snapshot := query.Result

	// expired snapshots should also be removed from db
	if snapshot.Expires.Before(time.Now()) {
		c.JsonApiErr(404, "Dashboard snapshot not found", err)
		return
	}

	dto := dtos.DashboardFullWithMeta{
		Dashboard: snapshot.Dashboard,
		Meta: dtos.DashboardMeta{
			Type:       m.DashTypeSnapshot,
			IsSnapshot: true,
			Created:    snapshot.Created,
			Expires:    snapshot.Expires,
		},
	}

	metrics.M_Api_Dashboard_Snapshot_Get.Inc()

	c.Resp.Header().Set("Cache-Control", "public, max-age=3600")
	c.JSON(200, dto)
}

func deleteExternalDashboardSnapshot(deleteKey string, externalUrl string) error {
	url, err := url.Parse(externalUrl)
	if err != nil {
		return fmt.Errorf("Invalid external snapshot url %s", externalUrl)
	}

	url.RawQuery = ""
	url.Fragment = ""
	url.Path = "/api/snapshots-delete/" + deleteKey

	response, err := http.Get(url.String())
	acceptedStatusCodes := map[int]bool{
		200: true,
		404: true,
	}

	if response != nil {
		defer response.Body.Close()
	}

	if err != nil {
		return err
	} else if _, ok := acceptedStatusCodes[response.StatusCode]; !ok {
		return fmt.Errorf("External snapshot response status code %d", response.StatusCode)
	}

	return nil
}

// GET /api/snapshots-delete/:deleteKey
func DeleteDashboardSnapshotByDeleteKey(c *m.ReqContext) Response {
	key := c.Params(":deleteKey")

	query := &m.GetDashboardSnapshotQuery{DeleteKey: key}

	err := bus.Dispatch(query)
	if err != nil {
		return Error(500, "Failed to get dashboard snapshot", err)
	}

	if query.Result == nil {
		return Error(404, "Dashboard snapshot not found", nil)
	}

	if query.Result.External {
		err := deleteExternalDashboardSnapshot(query.Result.DeleteKey, query.Result.ExternalUrl)
		if err != nil {
			return Error(500, "Failed to delete external dashboard", err)
		}
	}

	cmd := &m.DeleteDashboardSnapshotCommand{DeleteKey: query.Result.DeleteKey}

	if err := bus.Dispatch(cmd); err != nil {
		return Error(500, "Failed to delete dashboard snapshot", err)
	}

	return JSON(200, util.DynMap{"message": "Snapshot deleted. It might take an hour before it's cleared from any CDN caches."})
}

// DELETE /api/snapshots/:key
func DeleteDashboardSnapshot(c *m.ReqContext) Response {
	key := c.Params(":key")

	query := &m.GetDashboardSnapshotQuery{Key: key}

	err := bus.Dispatch(query)
	if err != nil {
		return Error(500, "Failed to get dashboard snapshot", err)
	}

	if query.Result == nil {
		return Error(404, "Failed to get dashboard snapshot", nil)
	}
	dashboard := query.Result.Dashboard
	dashboardID := dashboard.Get("id").MustInt64()

	guardian := guardian.New(dashboardID, c.OrgId, c.SignedInUser)
	canEdit, err := guardian.CanEdit()
	if err != nil {
		return Error(500, "Error while checking permissions for snapshot", err)
	}

	if !canEdit && query.Result.UserId != c.SignedInUser.UserId {
		return Error(403, "Access denied to this snapshot", nil)
	}

	if query.Result.External {
		err := deleteExternalDashboardSnapshot(query.Result.DeleteKey, query.Result.ExternalUrl)
		if err != nil {
			return Error(500, "Failed to delete external dashboard", err)
		}
	}

	cmd := &m.DeleteDashboardSnapshotCommand{DeleteKey: query.Result.DeleteKey}

	if err := bus.Dispatch(cmd); err != nil {
		return Error(500, "Failed to delete dashboard snapshot", err)
	}

	return JSON(200, util.DynMap{"message": "Snapshot deleted. It might take an hour before it's cleared from any CDN caches."})
}

// GET /api/dashboard/snapshots
func SearchDashboardSnapshots(c *m.ReqContext) Response {
	query := c.Query("query")
	limit := c.QueryInt("limit")

	if limit == 0 {
		limit = 1000
	}

	searchQuery := m.GetDashboardSnapshotsQuery{
		Name:         query,
		Limit:        limit,
		OrgId:        c.OrgId,
		SignedInUser: c.SignedInUser,
	}

	err := bus.Dispatch(&searchQuery)
	if err != nil {
		return Error(500, "Search failed", err)
	}

	dtos := make([]*m.DashboardSnapshotDTO, len(searchQuery.Result))
	for i, snapshot := range searchQuery.Result {
		dtos[i] = &m.DashboardSnapshotDTO{
			Id:          snapshot.Id,
			Name:        snapshot.Name,
			Key:         snapshot.Key,
			OrgId:       snapshot.OrgId,
			UserId:      snapshot.UserId,
			External:    snapshot.External,
			ExternalUrl: snapshot.ExternalUrl,
			Expires:     snapshot.Expires,
			Created:     snapshot.Created,
			Updated:     snapshot.Updated,
		}
	}

	return JSON(200, dtos)
}
