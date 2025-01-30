/*
Copyright 2017 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package cloudflare

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	cloudflare "github.com/cloudflare/cloudflare-go"
	log "github.com/sirupsen/logrus"

	"sigs.k8s.io/external-dns/endpoint"
	"sigs.k8s.io/external-dns/plan"
	"sigs.k8s.io/external-dns/provider"
	"sigs.k8s.io/external-dns/source"
)

const (
	// cloudFlareCreate is a ChangeAction enum value
	cloudFlareCreate = "CREATE"
	// cloudFlareDelete is a ChangeAction enum value
	cloudFlareDelete = "DELETE"
	// cloudFlareUpdate is a ChangeAction enum value
	cloudFlareUpdate = "UPDATE"
	// defaultCloudFlareRecordTTL 1 = automatic
	defaultCloudFlareRecordTTL = 1
)

// We have to use pointers to bools now, as the upstream cloudflare-go library requires them
// see: https://github.com/cloudflare/cloudflare-go/pull/595

// proxyEnabled is a pointer to a bool true showing the record should be proxied through cloudflare
var proxyEnabled *bool = boolPtr(true)

// proxyDisabled is a pointer to a bool false showing the record should not be proxied through cloudflare
var proxyDisabled *bool = boolPtr(false)

var recordTypeProxyNotSupported = map[string]bool{
	"LOC": true,
	"MX":  true,
	"NS":  true,
	"SPF": true,
	"TXT": true,
	"SRV": true,
}

// cloudFlareDNS is the subset of the CloudFlare API that we actually use.  Add methods as required. Signatures must match exactly.
type cloudFlareDNS interface {
	UserDetails(ctx context.Context) (cloudflare.User, error)
	ZoneIDByName(zoneName string) (string, error)
	ListZones(ctx context.Context, zoneID ...string) ([]cloudflare.Zone, error)
	ListZonesContext(ctx context.Context, opts ...cloudflare.ReqOption) (cloudflare.ZonesResponse, error)
	ZoneDetails(ctx context.Context, zoneID string) (cloudflare.Zone, error)
	ListDNSRecords(ctx context.Context, rc *cloudflare.ResourceContainer, rp cloudflare.ListDNSRecordsParams) ([]cloudflare.DNSRecord, *cloudflare.ResultInfo, error)
	CreateDNSRecord(ctx context.Context, rc *cloudflare.ResourceContainer, rp cloudflare.CreateDNSRecordParams) (cloudflare.DNSRecord, error)
	DeleteDNSRecord(ctx context.Context, rc *cloudflare.ResourceContainer, recordID string) error
	UpdateDNSRecord(ctx context.Context, rc *cloudflare.ResourceContainer, rp cloudflare.UpdateDNSRecordParams) error
	UpdateDataLocalizationRegionalHostname(ctx context.Context, rc *cloudflare.ResourceContainer, rp cloudflare.UpdateDataLocalizationRegionalHostnameParams) error
}

type zoneService struct {
	service *cloudflare.API
}

func (z zoneService) UserDetails(ctx context.Context) (cloudflare.User, error) {
	return z.service.UserDetails(ctx)
}

func (z zoneService) ListZones(ctx context.Context, zoneID ...string) ([]cloudflare.Zone, error) {
	return z.service.ListZones(ctx, zoneID...)
}

func (z zoneService) ZoneIDByName(zoneName string) (string, error) {
	return z.service.ZoneIDByName(zoneName)
}

func (z zoneService) CreateDNSRecord(ctx context.Context, rc *cloudflare.ResourceContainer, rp cloudflare.CreateDNSRecordParams) (cloudflare.DNSRecord, error) {
	return z.service.CreateDNSRecord(ctx, rc, rp)
}

func (z zoneService) ListDNSRecords(ctx context.Context, rc *cloudflare.ResourceContainer, rp cloudflare.ListDNSRecordsParams) ([]cloudflare.DNSRecord, *cloudflare.ResultInfo, error) {
	return z.service.ListDNSRecords(ctx, rc, rp)
}

func (z zoneService) UpdateDNSRecord(ctx context.Context, rc *cloudflare.ResourceContainer, rp cloudflare.UpdateDNSRecordParams) error {
	_, err := z.service.UpdateDNSRecord(ctx, rc, rp)
	return err
}

func (z zoneService) UpdateDataLocalizationRegionalHostname(ctx context.Context, rc *cloudflare.ResourceContainer, rp cloudflare.UpdateDataLocalizationRegionalHostnameParams) error {
	_, err := z.service.UpdateDataLocalizationRegionalHostname(ctx, rc, rp)
	return err
}

func (z zoneService) DeleteDNSRecord(ctx context.Context, rc *cloudflare.ResourceContainer, recordID string) error {
	return z.service.DeleteDNSRecord(ctx, rc, recordID)
}

func (z zoneService) ListZonesContext(ctx context.Context, opts ...cloudflare.ReqOption) (cloudflare.ZonesResponse, error) {
	return z.service.ListZonesContext(ctx, opts...)
}

func (z zoneService) ZoneDetails(ctx context.Context, zoneID string) (cloudflare.Zone, error) {
	return z.service.ZoneDetails(ctx, zoneID)
}

// CloudFlareProvider is an implementation of Provider for CloudFlare DNS.
type CloudFlareProvider struct {
	provider.BaseProvider
	Client cloudFlareDNS
	// only consider hosted zones managing domains ending in this suffix
	domainFilter      endpoint.DomainFilter
	zoneIDFilter      provider.ZoneIDFilter
	proxiedByDefault  bool
	DryRun            bool
	DNSRecordsPerPage int
	RegionKey         string
}

// cloudFlareChange differentiates between ChangActions
type cloudFlareChange struct {
	Action           string
	ResourceRecord   cloudflare.DNSRecord
	RegionalHostname cloudflare.RegionalHostname
}

// RecordParamsTypes is a typeset of the possible Record Params that can be passed to cloudflare-go library
type RecordParamsTypes interface {
	cloudflare.UpdateDNSRecordParams | cloudflare.CreateDNSRecordParams
}

// updateDNSRecordParam is a function that returns the appropriate Record Param based on the cloudFlareChange passed in
func updateDNSRecordParam(cfc cloudFlareChange) cloudflare.UpdateDNSRecordParams {
	return cloudflare.UpdateDNSRecordParams{
		Name:    cfc.ResourceRecord.Name,
		TTL:     cfc.ResourceRecord.TTL,
		Proxied: cfc.ResourceRecord.Proxied,
		Type:    cfc.ResourceRecord.Type,
		Content: cfc.ResourceRecord.Content,
	}
}

// updateDataLocalizationRegionalHostnameParams is a function that returns the appropriate RegionalHostname Param based on the cloudFlareChange passed in
func updateDataLocalizationRegionalHostnameParams(cfc cloudFlareChange) cloudflare.UpdateDataLocalizationRegionalHostnameParams {
	return cloudflare.UpdateDataLocalizationRegionalHostnameParams{
		Hostname:  cfc.RegionalHostname.Hostname,
		RegionKey: cfc.RegionalHostname.RegionKey,
	}
}

// getCreateDNSRecordParam is a function that returns the appropriate Record Param based on the cloudFlareChange passed in
func getCreateDNSRecordParam(cfc cloudFlareChange) cloudflare.CreateDNSRecordParams {
	return cloudflare.CreateDNSRecordParams{
		Name:    cfc.ResourceRecord.Name,
		TTL:     cfc.ResourceRecord.TTL,
		Proxied: cfc.ResourceRecord.Proxied,
		Type:    cfc.ResourceRecord.Type,
		Content: cfc.ResourceRecord.Content,
	}
}

// NewCloudFlareProvider initializes a new CloudFlare DNS based Provider.
func NewCloudFlareProvider(domainFilter endpoint.DomainFilter, zoneIDFilter provider.ZoneIDFilter, proxiedByDefault bool, dryRun bool, dnsRecordsPerPage int, regionKey string) (*CloudFlareProvider, error) {
	// initialize via chosen auth method and returns new API object
	var (
		config *cloudflare.API
		err    error
	)
	if os.Getenv("CF_API_TOKEN") != "" {
		token := os.Getenv("CF_API_TOKEN")
		if strings.HasPrefix(token, "file:") {
			tokenBytes, err := os.ReadFile(strings.TrimPrefix(token, "file:"))
			if err != nil {
				return nil, fmt.Errorf("failed to read CF_API_TOKEN from file: %w", err)
			}
			token = strings.TrimSpace(string(tokenBytes))
		}
		config, err = cloudflare.NewWithAPIToken(token)
	} else {
		config, err = cloudflare.New(os.Getenv("CF_API_KEY"), os.Getenv("CF_API_EMAIL"))
	}
	if err != nil {
		return nil, fmt.Errorf("failed to initialize cloudflare provider: %v", err)
	}
	provider := &CloudFlareProvider{
		// Client: config,
		Client:            zoneService{config},
		domainFilter:      domainFilter,
		zoneIDFilter:      zoneIDFilter,
		proxiedByDefault:  proxiedByDefault,
		DryRun:            dryRun,
		DNSRecordsPerPage: dnsRecordsPerPage,
		RegionKey:         regionKey,
	}
	return provider, nil
}

// Zones returns the list of hosted zones.
func (p *CloudFlareProvider) Zones(ctx context.Context) ([]cloudflare.Zone, error) {
	result := []cloudflare.Zone{}

	// if there is a zoneIDfilter configured
	// && if the filter isn't just a blank string (used in tests)
	if len(p.zoneIDFilter.ZoneIDs) > 0 && p.zoneIDFilter.ZoneIDs[0] != "" {
		log.Debugln("zoneIDFilter configured. only looking up zone IDs defined")
		for _, zoneID := range p.zoneIDFilter.ZoneIDs {
			log.Debugf("looking up zone %s", zoneID)
			detailResponse, err := p.Client.ZoneDetails(ctx, zoneID)
			if err != nil {
				log.Errorf("zone %s lookup failed, %v", zoneID, err)
				return result, err
			}
			log.WithFields(log.Fields{
				"zoneName": detailResponse.Name,
				"zoneID":   detailResponse.ID,
			}).Debugln("adding zone for consideration")
			result = append(result, detailResponse)
		}
		return result, nil
	}

	log.Debugln("no zoneIDFilter configured, looking at all zones")

	zonesResponse, err := p.Client.ListZonesContext(ctx)
	if err != nil {
		var apiErr *cloudflare.Error
		if errors.As(err, &apiErr) {
			if apiErr.ClientRateLimited() || apiErr.StatusCode >= http.StatusInternalServerError {
				// Handle rate limit error as a soft error
				return nil, provider.NewSoftError(err)
			}
		}
		return nil, err
	}

	for _, zone := range zonesResponse.Result {
		if !p.domainFilter.Match(zone.Name) {
			log.Debugf("zone %s not in domain filter", zone.Name)
			continue
		}
		result = append(result, zone)
	}

	return result, nil
}

// Records returns the list of records.
func (p *CloudFlareProvider) Records(ctx context.Context) ([]*endpoint.Endpoint, error) {
	zones, err := p.Zones(ctx)
	if err != nil {
		return nil, err
	}

	endpoints := []*endpoint.Endpoint{}
	for _, zone := range zones {
		records, err := p.listDNSRecordsWithAutoPagination(ctx, zone.ID)
		if err != nil {
			return nil, err
		}

		// As CloudFlare does not support "sets" of targets, but instead returns
		// a single entry for each name/type/target, we have to group by name
		// and record to allow the planner to calculate the correct plan. See #992.
		endpoints = append(endpoints, groupByNameAndType(records)...)
	}

	return endpoints, nil
}

// ApplyChanges applies a given set of changes in a given zone.
func (p *CloudFlareProvider) ApplyChanges(ctx context.Context, changes *plan.Changes) error {
	cloudflareChanges := []*cloudFlareChange{}

	for _, endpoint := range changes.Create {
		for _, target := range endpoint.Targets {
			cloudflareChanges = append(cloudflareChanges, p.newCloudFlareChange(cloudFlareCreate, endpoint, target))
		}
	}

	for i, desired := range changes.UpdateNew {
		current := changes.UpdateOld[i]

		add, remove, leave := provider.Difference(current.Targets, desired.Targets)

		for _, a := range remove {
			cloudflareChanges = append(cloudflareChanges, p.newCloudFlareChange(cloudFlareDelete, current, a))
		}

		for _, a := range add {
			cloudflareChanges = append(cloudflareChanges, p.newCloudFlareChange(cloudFlareCreate, desired, a))
		}

		for _, a := range leave {
			cloudflareChanges = append(cloudflareChanges, p.newCloudFlareChange(cloudFlareUpdate, desired, a))
		}
	}

	for _, endpoint := range changes.Delete {
		for _, target := range endpoint.Targets {
			cloudflareChanges = append(cloudflareChanges, p.newCloudFlareChange(cloudFlareDelete, endpoint, target))
		}
	}

	return p.submitChanges(ctx, cloudflareChanges)
}

// submitChanges takes a zone and a collection of Changes and sends them as a single transaction.
func (p *CloudFlareProvider) submitChanges(ctx context.Context, changes []*cloudFlareChange) error {
	// return early if there is nothing to change
	if len(changes) == 0 {
		log.Info("All records are already up to date")
		return nil
	}

	zones, err := p.Zones(ctx)
	if err != nil {
		return err
	}
	// separate into per-zone change sets to be passed to the API.
	changesByZone := p.changesByZone(zones, changes)

	var failedZones []string
	for zoneID, changes := range changesByZone {
		records, err := p.listDNSRecordsWithAutoPagination(ctx, zoneID)
		if err != nil {
			return fmt.Errorf("could not fetch records from zone, %v", err)
		}

		var failedChange bool
		for _, change := range changes {
			logFields := log.Fields{
				"record": change.ResourceRecord.Name,
				"type":   change.ResourceRecord.Type,
				"ttl":    change.ResourceRecord.TTL,
				"action": change.Action,
				"zone":   zoneID,
			}

			log.WithFields(logFields).Info("Changing record.")

			if p.DryRun {
				continue
			}

			resourceContainer := cloudflare.ZoneIdentifier(zoneID)
			if change.Action == cloudFlareUpdate {
				recordID := p.getRecordID(records, change.ResourceRecord)
				if recordID == "" {
					log.WithFields(logFields).Errorf("failed to find previous record: %v", change.ResourceRecord)
					continue
				}
				recordParam := updateDNSRecordParam(*change)
				regionalHostnameParam := updateDataLocalizationRegionalHostnameParams(*change)
				recordParam.ID = recordID
				err := p.Client.UpdateDNSRecord(ctx, resourceContainer, recordParam)
				if err != nil {
					failedChange = true
					log.WithFields(logFields).Errorf("failed to update record: %v", err)
				}
				regionalHostnameErr := p.Client.UpdateDataLocalizationRegionalHostname(ctx, resourceContainer, regionalHostnameParam)
				if regionalHostnameErr != nil {
					failedChange = true
					log.WithFields(logFields).Errorf("failed to update record when editing region: %v", regionalHostnameErr)
				}
			} else if change.Action == cloudFlareDelete {
				recordID := p.getRecordID(records, change.ResourceRecord)
				if recordID == "" {
					log.WithFields(logFields).Errorf("failed to find previous record: %v", change.ResourceRecord)
					continue
				}
				err := p.Client.DeleteDNSRecord(ctx, resourceContainer, recordID)
				if err != nil {
					failedChange = true
					log.WithFields(logFields).Errorf("failed to delete record: %v", err)
				}
			} else if change.Action == cloudFlareCreate {
				recordParam := getCreateDNSRecordParam(*change)
				_, err := p.Client.CreateDNSRecord(ctx, resourceContainer, recordParam)
				if err != nil {
					failedChange = true
					log.WithFields(logFields).Errorf("failed to create record: %v", err)
				}
			}
		}

		if failedChange {
			failedZones = append(failedZones, zoneID)
		}
	}

	if len(failedZones) > 0 {
		return fmt.Errorf("failed to submit all changes for the following zones: %v", failedZones)
	}

	return nil
}

// AdjustEndpoints modifies the endpoints as needed by the specific provider
func (p *CloudFlareProvider) AdjustEndpoints(endpoints []*endpoint.Endpoint) ([]*endpoint.Endpoint, error) {
	adjustedEndpoints := []*endpoint.Endpoint{}
	for _, e := range endpoints {
		proxied := shouldBeProxied(e, p.proxiedByDefault)
		if proxied {
			e.RecordTTL = 0
		}
		e.SetProviderSpecificProperty(source.CloudflareProxiedKey, strconv.FormatBool(proxied))

		adjustedEndpoints = append(adjustedEndpoints, e)
	}
	return adjustedEndpoints, nil
}

// changesByZone separates a multi-zone change into a single change per zone.
func (p *CloudFlareProvider) changesByZone(zones []cloudflare.Zone, changeSet []*cloudFlareChange) map[string][]*cloudFlareChange {
	changes := make(map[string][]*cloudFlareChange)
	zoneNameIDMapper := provider.ZoneIDName{}

	for _, z := range zones {
		zoneNameIDMapper.Add(z.ID, z.Name)
		changes[z.ID] = []*cloudFlareChange{}
	}

	for _, c := range changeSet {
		zoneID, _ := zoneNameIDMapper.FindZone(c.ResourceRecord.Name)
		if zoneID == "" {
			log.Debugf("Skipping record %s because no hosted zone matching record DNS Name was detected", c.ResourceRecord.Name)
			continue
		}
		changes[zoneID] = append(changes[zoneID], c)
	}

	return changes
}

func (p *CloudFlareProvider) getRecordID(records []cloudflare.DNSRecord, record cloudflare.DNSRecord) string {
	for _, zoneRecord := range records {
		if zoneRecord.Name == record.Name && zoneRecord.Type == record.Type && zoneRecord.Content == record.Content {
			return zoneRecord.ID
		}
	}
	return ""
}

func (p *CloudFlareProvider) newCloudFlareChange(action string, endpoint *endpoint.Endpoint, target string) *cloudFlareChange {
	ttl := defaultCloudFlareRecordTTL
	proxied := shouldBeProxied(endpoint, p.proxiedByDefault)

	if endpoint.RecordTTL.IsConfigured() {
		ttl = int(endpoint.RecordTTL)
	}
	dt := time.Now()
	return &cloudFlareChange{
		Action: action,
		ResourceRecord: cloudflare.DNSRecord{
			Name:    endpoint.DNSName,
			TTL:     ttl,
			Proxied: &proxied,
			Type:    endpoint.RecordType,
			Content: target,
			Meta: map[string]interface{}{
				"region": p.RegionKey,
			},
		},
		RegionalHostname: cloudflare.RegionalHostname{
			Hostname:  endpoint.DNSName,
			RegionKey: p.RegionKey,
			CreatedOn: &dt,
		},
	}
}

// listDNSRecords performs automatic pagination of results on requests to cloudflare.ListDNSRecords with custom per_page values
func (p *CloudFlareProvider) listDNSRecordsWithAutoPagination(ctx context.Context, zoneID string) ([]cloudflare.DNSRecord, error) {
	var records []cloudflare.DNSRecord
	resultInfo := cloudflare.ResultInfo{PerPage: p.DNSRecordsPerPage, Page: 1}
	params := cloudflare.ListDNSRecordsParams{ResultInfo: resultInfo}
	for {
		pageRecords, resultInfo, err := p.Client.ListDNSRecords(ctx, cloudflare.ZoneIdentifier(zoneID), params)
		if err != nil {
			var apiErr *cloudflare.Error
			if errors.As(err, &apiErr) {
				if apiErr.ClientRateLimited() || apiErr.StatusCode >= http.StatusInternalServerError {
					// Handle rate limit error as a soft error
					return nil, provider.NewSoftError(err)
				}
			}
			return nil, err
		}

		records = append(records, pageRecords...)
		params.ResultInfo = resultInfo.Next()
		if params.ResultInfo.Done() {
			break
		}
	}
	return records, nil
}

func shouldBeProxied(endpoint *endpoint.Endpoint, proxiedByDefault bool) bool {
	proxied := proxiedByDefault

	for _, v := range endpoint.ProviderSpecific {
		if v.Name == source.CloudflareProxiedKey {
			b, err := strconv.ParseBool(v.Value)
			if err != nil {
				log.Errorf("Failed to parse annotation [%s]: %v", source.CloudflareProxiedKey, err)
			} else {
				proxied = b
			}
			break
		}
	}

	if recordTypeProxyNotSupported[endpoint.RecordType] {
		proxied = false
	}
	return proxied
}

func groupByNameAndType(records []cloudflare.DNSRecord) []*endpoint.Endpoint {
	endpoints := []*endpoint.Endpoint{}

	// group supported records by name and type
	groups := map[string][]cloudflare.DNSRecord{}

	for _, r := range records {
		if !provider.SupportedRecordType(r.Type) {
			continue
		}

		groupBy := r.Name + r.Type
		if _, ok := groups[groupBy]; !ok {
			groups[groupBy] = []cloudflare.DNSRecord{}
		}

		groups[groupBy] = append(groups[groupBy], r)
	}

	// create single endpoint with all the targets for each name/type
	for _, records := range groups {
		targets := make([]string, len(records))
		for i, record := range records {
			targets[i] = record.Content
		}
		endpoints = append(endpoints,
			endpoint.NewEndpointWithTTL(
				records[0].Name,
				records[0].Type,
				endpoint.TTL(records[0].TTL),
				targets...).
				WithProviderSpecific(source.CloudflareProxiedKey, strconv.FormatBool(*records[0].Proxied)),
		)
	}

	return endpoints
}

// boolPtr is used as a helper function to return a pointer to a boolean
// Needed because some parameters require a pointer.
func boolPtr(b bool) *bool {
	return &b
}
