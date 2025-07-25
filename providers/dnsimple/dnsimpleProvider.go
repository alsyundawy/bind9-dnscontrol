package dnsimple

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/StackExchange/dnscontrol/v4/models"
	"github.com/StackExchange/dnscontrol/v4/pkg/diff"
	"github.com/StackExchange/dnscontrol/v4/pkg/printer"
	"github.com/StackExchange/dnscontrol/v4/pkg/txtutil"
	"github.com/StackExchange/dnscontrol/v4/providers"
	dnsimpleapi "github.com/dnsimple/dnsimple-go/dnsimple"
	"golang.org/x/oauth2"
)

var features = providers.DocumentationNotes{
	// The default for unlisted capabilities is 'Cannot'.
	// See providers/capabilities.go for the entire list of capabilities.
	providers.CanAutoDNSSEC:          providers.Can(),
	providers.CanGetZones:            providers.Can(),
	providers.CanConcur:              providers.Can(),
	providers.CanUseAlias:            providers.Can(),
	providers.CanUseCAA:              providers.Can(),
	providers.CanUseDS:               providers.Cannot(),
	providers.CanUseDSForChildren:    providers.Cannot(),
	providers.CanUseLOC:              providers.Cannot(),
	providers.CanUseNAPTR:            providers.Can(),
	providers.CanUsePTR:              providers.Can(),
	providers.CanUseSRV:              providers.Can(),
	providers.CanUseSSHFP:            providers.Can(),
	providers.CanUseTLSA:             providers.Cannot(),
	providers.DocCreateDomains:       providers.Cannot(),
	providers.DocDualHost:            providers.Cannot("DNSimple does not allow sufficient control over the apex NS records"),
	providers.DocOfficiallySupported: providers.Cannot(),
}

func init() {
	const providerName = "DNSIMPLE"
	const providerMaintainer = "@onlyhavecans"
	providers.RegisterRegistrarType(providerName, newReg)
	fns := providers.DspFuncs{
		Initializer:   newDsp,
		RecordAuditor: AuditRecords,
	}
	providers.RegisterDomainServiceProviderType(providerName, fns, features)
	providers.RegisterMaintainer(providerName, providerMaintainer)
}

const stateRegistered = "registered"

var defaultNameServerNames = []string{
	"ns1.dnsimple.com",
	"ns2.dnsimple-edge.net",
	"ns3.dnsimple.com",
	"ns4.dnsimple-edge.org",
}

var nameServerSuffixes = []string{
	".dnsimple.com.",
	".dnsimple-edge.org.",
	".dnsimple-edge.net.",
	".dnsimple-edge.io.",
	".dnsimple-edge.com.",
}

// dnsimpleProvider is the handle for this provider.
type dnsimpleProvider struct {
	AccountToken string // The account access token
	BaseURL      string // An alternate base URI

	// We can have multiple _distinct_ versions of this struct, authenticated to
	// different accounts, so _each_ version needs to be initialized just once.
	// Note that sync.Once contains a reference to sync.noCopy so this will cause
	// `go vet` to catch attempts to copy this outer provider struct.
	onceFetchAccountId sync.Once

	// This is protected under onceFetchAccountId so that this is fully safe concurrently.
	accountID string // Account id cache
}

// GetNameservers returns the name servers for a domain.
func (c *dnsimpleProvider) GetNameservers(_ string) ([]*models.Nameserver, error) {
	return models.ToNameservers(defaultNameServerNames)
}

// GetZoneRecords gets the records of a zone and returns them in RecordConfig format.
func (c *dnsimpleProvider) GetZoneRecords(domain string, meta map[string]string) (models.Records, error) {
	records, err := c.getRecords(domain)
	if err != nil {
		return nil, err
	}

	var cleanedRecords models.Records
	for _, r := range records {
		if r.Type == "SOA" {
			continue
		}

		if r.Name == "" {
			r.Name = "@"
		}

		if r.Type == "CNAME" || r.Type == "ALIAS" || r.Type == "NS" {
			r.Content += "."
		} else if r.Type == "MX" && r.Content != "." {
			r.Content += "."
		}

		// DNSimple adds TXT records that mirror the alias records.
		// They manage them on ALIAS updates, so pretend they don't exist
		if r.Type == "TXT" && strings.HasPrefix(r.Content, `"ALIAS for `) {
			continue
		}
		// This second check is the same of before, but it exists for compatibility purpose.
		// Until Nov 2023 DNSimple did not normalize TXT records, and they used to store TXT records without quotes.
		//
		// This is a backward-compatible function to facilitate the TXT transition.
		if r.Type == "TXT" && strings.HasPrefix(r.Content, `ALIAS for `) {
			continue
		}

		rec := &models.RecordConfig{
			TTL:      uint32(r.TTL),
			Original: r,
		}
		rec.SetLabel(r.Name, domain)

		var err error
		switch rtype := r.Type; rtype {
		case "DNSKEY", "CDNSKEY", "CDS":
			continue
		case "ALIAS", "URL":
			rec.Type = r.Type
			err = rec.SetTarget(r.Content)
		case "DS":
			err = rec.SetTargetDSString(r.Content)
		case "MX":
			err = rec.SetTargetMX(uint16(r.Priority), r.Content)
		case "SRV":
			err = rec.SetTargetSRVPriorityString(uint16(r.Priority), r.Content)
		case "TXT":
			// This is a backward-compatible function to facilitate the TXT transition.
			if isQuotedTXT(r.Content) {
				err = rec.PopulateFromStringFunc(r.Type, r.Content, domain, txtutil.ParseQuoted)
			} else {
				err = rec.SetTargetTXT("legacy: " + r.Content)
			}
		default:
			err = rec.PopulateFromString(r.Type, r.Content, domain)
		}

		if err != nil {
			return nil, fmt.Errorf("unparsable record received from dnsimple: %w", err)
		}

		cleanedRecords = append(cleanedRecords, rec)
	}

	// Apex NS are immutable via API
	cleanedRecords = removeApexNS(cleanedRecords)

	return cleanedRecords, nil
}

func (c *dnsimpleProvider) GetZoneRecordsCorrections(dc *models.DomainConfig, actual models.Records) ([]*models.Correction, int, error) {
	removeOtherApexNS(dc)

	dnssecFixes, err := c.getDNSSECCorrections(dc)
	if err != nil {
		return nil, 0, err
	}

	toReport, create, del, modify, actualChangeCount, err := diff.NewCompat(dc).IncrementalDiff(actual)
	if err != nil {
		return nil, 0, err
	}
	// Start corrections with the reports
	corrections := diff.GenerateMessageCorrections(toReport)
	// Next dnsSec fixes
	corrections = append(corrections, dnssecFixes...)

	for _, del := range del {
		rec := del.Existing.Original.(dnsimpleapi.ZoneRecord)
		corrections = append(corrections, &models.Correction{
			Msg: del.String(),
			F:   c.deleteRecordFunc(rec.ID, dc.Name),
		})
	}

	for _, cre := range create {
		rec := cre.Desired
		corrections = append(corrections, &models.Correction{
			Msg: cre.String(),
			F:   c.createRecordFunc(rec, dc.Name),
		})
	}

	for _, mod := range modify {
		old := mod.Existing.Original.(dnsimpleapi.ZoneRecord)
		rec := mod.Desired
		corrections = append(corrections, &models.Correction{
			Msg: mod.String(),
			F:   c.updateRecordFunc(&old, rec, dc.Name),
		})
	}

	return corrections, actualChangeCount, nil
}

func removeApexNS(records models.Records) models.Records {
	var filtered models.Records
	for _, r := range records {
		if r.Type == "NS" && r.Name == "@" {
			continue
		}
		filtered = append(filtered, r)
	}
	return filtered
}

// GetRegistrarCorrections returns corrections that update a domain's registrar.
func (c *dnsimpleProvider) GetRegistrarCorrections(dc *models.DomainConfig) ([]*models.Correction, error) {
	var corrections []*models.Correction

	nameServers, err := c.getNameservers(dc.Name)
	if err != nil {
		return nil, err
	}
	sort.Strings(nameServers)

	actual := strings.Join(nameServers, ",")

	var expectedSet []string
	for _, ns := range dc.Nameservers {
		expectedSet = append(expectedSet, ns.Name)
	}
	sort.Strings(expectedSet)
	expected := strings.Join(expectedSet, ",")

	if actual != expected {
		return []*models.Correction{
			{
				Msg: fmt.Sprintf("Update nameservers %s -> %s", actual, expected),
				F:   c.updateNameserversFunc(expectedSet, dc.Name),
			},
		}, nil
	}

	return corrections, nil
}

// getDNSSECCorrections returns corrections that update a domain's DNSSEC state.
func (c *dnsimpleProvider) getDNSSECCorrections(dc *models.DomainConfig) ([]*models.Correction, error) {
	enabled, err := c.getDnssec(dc.Name)
	if err != nil {
		return nil, err
	}

	if enabled && dc.AutoDNSSEC == "off" {
		return []*models.Correction{
			{
				Msg: "Disable DNSSEC",
				F:   func() error { _, err := c.disableDnssec(dc.Name); return err },
			},
		}, nil
	}

	if !enabled && dc.AutoDNSSEC == "on" {
		return []*models.Correction{
			{
				Msg: "Enable DNSSEC",
				F:   func() error { _, err := c.enableDnssec(dc.Name); return err },
			},
		}, nil
	}

	return []*models.Correction{}, nil
}

// DNSimple calls

// Initializes a new DNSimple API client.
//
// - if BaseURL is present, the provided BaseURL is used. Useful to switch to DNSimple sandbox site. It defaults to production otherwise.
// - if "DNSIMPLE_DEBUG_HTTP" is set to "1", it enables the API client logging.
func (c *dnsimpleProvider) getClient() *dnsimpleapi.Client {
	ts := oauth2.StaticTokenSource(&oauth2.Token{AccessToken: c.AccountToken})
	tc := oauth2.NewClient(context.Background(), ts)

	// new client
	client := dnsimpleapi.NewClient(tc)
	client.SetUserAgent("DNSControl")

	if c.BaseURL != "" {
		client.BaseURL = c.BaseURL
	}
	if os.Getenv("DNSIMPLE_DEBUG_HTTP") == "1" {
		client.Debug = true
	}
	return client
}

func (c *dnsimpleProvider) getAccountID() (string, error) {
	var onceErr error
	c.onceFetchAccountId.Do(func() {
		client := c.getClient()
		whoamiResponse, err := client.Identity.Whoami(context.Background())
		if err != nil {
			onceErr = err
			return
		}
		if whoamiResponse.Data.User != nil && whoamiResponse.Data.Account == nil {
			onceErr = errors.New("DNSimple token appears to be a user token. Please supply an account token")
			return
		}
		c.accountID = strconv.FormatInt(whoamiResponse.Data.Account.ID, 10)
	})
	if onceErr != nil {
		return "", onceErr
	}
	return c.accountID, nil
}

func (c *dnsimpleProvider) getRecords(domainName string) ([]dnsimpleapi.ZoneRecord, error) {
	client := c.getClient()

	accountID, err := c.getAccountID()
	if err != nil {
		var errorResponse *dnsimpleapi.ErrorResponse
		if errors.As(err, &errorResponse) {
			return nil, compileAttributeErrors(errorResponse)
		}
		return nil, err
	}

	opts := &dnsimpleapi.ZoneRecordListOptions{}
	var recs []dnsimpleapi.ZoneRecord
	page := 1
	for {
		opts.Page = &page
		recordsResponse, err := client.Zones.ListRecords(context.Background(), accountID, domainName, opts)
		if err != nil {
			var errorResponse *dnsimpleapi.ErrorResponse
			if errors.As(err, &errorResponse) {
				return nil, compileAttributeErrors(errorResponse)
			}
			return nil, err
		}
		recs = append(recs, recordsResponse.Data...)
		pg := recordsResponse.Pagination
		if pg.CurrentPage == pg.TotalPages {
			break
		}
		page++
	}

	return recs, nil
}

func (c *dnsimpleProvider) getDnssec(domainName string) (bool, error) {
	var (
		client    *dnsimpleapi.Client
		accountID string
		err       error
	)
	client = c.getClient()
	if accountID, err = c.getAccountID(); err != nil {
		var errorResponse *dnsimpleapi.ErrorResponse
		if errors.As(err, &errorResponse) {
			return false, compileAttributeErrors(errorResponse)
		}
		return false, err
	}

	dnssecResponse, err := client.Domains.GetDnssec(context.Background(), accountID, domainName)
	if err != nil {
		var errorResponse *dnsimpleapi.ErrorResponse
		if errors.As(err, &errorResponse) {
			return false, compileAttributeErrors(errorResponse)
		}
		return false, err
	}
	if dnssecResponse.Data == nil {
		return false, nil
	}
	return dnssecResponse.Data.Enabled, nil
}

func (c *dnsimpleProvider) enableDnssec(domainName string) (bool, error) {
	var (
		client    *dnsimpleapi.Client
		accountID string
		err       error
	)
	client = c.getClient()
	if accountID, err = c.getAccountID(); err != nil {
		var errorResponse *dnsimpleapi.ErrorResponse
		if errors.As(err, &errorResponse) {
			return false, compileAttributeErrors(errorResponse)
		}
		return false, err
	}

	dnssecResponse, err := client.Domains.EnableDnssec(context.Background(), accountID, domainName)
	if err != nil {
		var errorResponse *dnsimpleapi.ErrorResponse
		if errors.As(err, &errorResponse) {
			return false, compileAttributeErrors(errorResponse)
		}
		return false, err
	}
	if dnssecResponse.Data == nil {
		return false, nil
	}
	return dnssecResponse.Data.Enabled, nil
}

func (c *dnsimpleProvider) disableDnssec(domainName string) (bool, error) {
	var (
		client    *dnsimpleapi.Client
		accountID string
		err       error
	)
	client = c.getClient()
	if accountID, err = c.getAccountID(); err != nil {
		var errorResponse *dnsimpleapi.ErrorResponse
		if errors.As(err, &errorResponse) {
			return false, compileAttributeErrors(errorResponse)
		}
		return false, err
	}

	dnssecResponse, err := client.Domains.DisableDnssec(context.Background(), accountID, domainName)
	if err != nil {
		var errorResponse *dnsimpleapi.ErrorResponse
		if errors.As(err, &errorResponse) {
			return false, compileAttributeErrors(errorResponse)
		}
		return false, err
	}
	if dnssecResponse.Data == nil {
		return false, nil
	}
	return dnssecResponse.Data.Enabled, nil
}

// Returns the name server names that should be used. If the domain is registered
// then this method will return the delegation name servers. If this domain
// is hosted only, then it will return the default DNSimple name servers.
func (c *dnsimpleProvider) getNameservers(domainName string) ([]string, error) {
	client := c.getClient()

	accountID, err := c.getAccountID()
	if err != nil {
		var errorResponse *dnsimpleapi.ErrorResponse
		if errors.As(err, &errorResponse) {
			return nil, compileAttributeErrors(errorResponse)
		}
		return nil, err
	}

	domainResponse, err := client.Domains.GetDomain(context.Background(), accountID, domainName)
	if err != nil {
		var errorResponse *dnsimpleapi.ErrorResponse
		if errors.As(err, &errorResponse) {
			return nil, compileAttributeErrors(errorResponse)
		}
		return nil, err
	}

	if domainResponse.Data.State == stateRegistered {
		delegationResponse, err := client.Registrar.GetDomainDelegation(context.Background(), accountID, domainName)
		if err != nil {
			var errorResponse *dnsimpleapi.ErrorResponse
			if errors.As(err, &errorResponse) {
				return nil, compileAttributeErrors(errorResponse)
			}
			return nil, err
		}

		return *delegationResponse.Data, nil
	}
	return defaultNameServerNames, nil
}

// Returns a function that can be invoked to change the delegation of the domain to the given name server names.
func (c *dnsimpleProvider) updateNameserversFunc(nameServerNames []string, domainName string) func() error {
	return func() error {
		client := c.getClient()

		accountID, err := c.getAccountID()
		if err != nil {
			var errorResponse *dnsimpleapi.ErrorResponse
			if errors.As(err, &errorResponse) {
				return compileAttributeErrors(errorResponse)
			}
			return err
		}

		nameServers := dnsimpleapi.Delegation(nameServerNames)

		_, err = client.Registrar.ChangeDomainDelegation(context.Background(), accountID, domainName, &nameServers)
		if err != nil {
			var errorResponse *dnsimpleapi.ErrorResponse
			if errors.As(err, &errorResponse) {
				return compileAttributeErrors(errorResponse)
			}
			return err
		}

		return nil
	}
}

// Returns a function that can be invoked to create a record in a zone.
func (c *dnsimpleProvider) createRecordFunc(rc *models.RecordConfig, domainName string) func() error {
	return func() error {
		client := c.getClient()

		accountID, err := c.getAccountID()
		if err != nil {
			var errorResponse *dnsimpleapi.ErrorResponse
			if errors.As(err, &errorResponse) {
				return compileAttributeErrors(errorResponse)
			}
			return err
		}
		record := dnsimpleapi.ZoneRecordAttributes{
			Name:     dnsimpleapi.String(rc.GetLabel()),
			Type:     rc.Type,
			Content:  getTargetRecordContent(rc),
			TTL:      int(rc.TTL),
			Priority: getTargetRecordPriority(rc),
		}
		_, err = client.Zones.CreateRecord(context.Background(), accountID, domainName, record)
		if err != nil {
			var errorResponse *dnsimpleapi.ErrorResponse
			if errors.As(err, &errorResponse) {
				return compileAttributeErrors(errorResponse)
			}
			return err
		}

		return nil
	}
}

// Returns a function that can be invoked to delete a record in a zone.
func (c *dnsimpleProvider) deleteRecordFunc(recordID int64, domainName string) func() error {
	return func() error {
		client := c.getClient()

		accountID, err := c.getAccountID()
		if err != nil {
			var errorResponse *dnsimpleapi.ErrorResponse
			if errors.As(err, &errorResponse) {
				return compileAttributeErrors(errorResponse)
			}
			return err
		}

		_, err = client.Zones.DeleteRecord(context.Background(), accountID, domainName, recordID)
		if err != nil {
			var errorResponse *dnsimpleapi.ErrorResponse
			if errors.As(err, &errorResponse) {
				return compileAttributeErrors(errorResponse)
			}
			return err
		}

		return nil
	}
}

// Returns a function that can be invoked to update a record in a zone.
func (c *dnsimpleProvider) updateRecordFunc(old *dnsimpleapi.ZoneRecord, rc *models.RecordConfig, domainName string) func() error {
	return func() error {
		client := c.getClient()

		accountID, err := c.getAccountID()
		if err != nil {
			var errorResponse *dnsimpleapi.ErrorResponse
			if errors.As(err, &errorResponse) {
				return compileAttributeErrors(errorResponse)
			}
			return err
		}

		record := dnsimpleapi.ZoneRecordAttributes{
			Name:     dnsimpleapi.String(rc.GetLabel()),
			Type:     rc.Type,
			Content:  getTargetRecordContent(rc),
			TTL:      int(rc.TTL),
			Priority: getTargetRecordPriority(rc),
		}

		_, err = client.Zones.UpdateRecord(context.Background(), accountID, domainName, old.ID, record)
		if err != nil {
			var errorResponse *dnsimpleapi.ErrorResponse
			if errors.As(err, &errorResponse) {
				return compileAttributeErrors(errorResponse)
			}
			return err
		}

		return nil
	}
}

// ListZones returns all the zones in an account
func (c *dnsimpleProvider) ListZones() ([]string, error) {
	client := c.getClient()
	accountID, err := c.getAccountID()
	if err != nil {
		var errorResponse *dnsimpleapi.ErrorResponse
		if errors.As(err, &errorResponse) {
			return nil, compileAttributeErrors(errorResponse)
		}
		return nil, err
	}

	var zones []string
	opts := &dnsimpleapi.ZoneListOptions{}
	page := 1
	for {
		opts.Page = &page
		zonesResponse, err := client.Zones.ListZones(context.Background(), accountID, opts)
		if err != nil {
			var errorResponse *dnsimpleapi.ErrorResponse
			if errors.As(err, &errorResponse) {
				return nil, compileAttributeErrors(errorResponse)
			}
			return nil, err
		}
		for _, zone := range zonesResponse.Data {
			zones = append(zones, zone.Name)
		}
		pg := zonesResponse.Pagination
		if pg.CurrentPage == pg.TotalPages {
			break
		}
		page++
	}
	return zones, nil
}

// constructors

func newReg(conf map[string]string) (providers.Registrar, error) {
	return newProvider(conf, nil)
}

func newDsp(conf map[string]string, metadata json.RawMessage) (providers.DNSServiceProvider, error) {
	return newProvider(conf, metadata)
}

func newProvider(m map[string]string, _ json.RawMessage) (*dnsimpleProvider, error) {
	api := &dnsimpleProvider{}
	api.AccountToken = m["token"]
	if api.AccountToken == "" {
		return nil, errors.New("missing DNSimple token")
	}

	if m["baseurl"] != "" {
		api.BaseURL = m["baseurl"]
	}

	return api, nil
}

// utilities

// Removes all non-dnsimple NS records from our desired state.
// If any are found, print a warning.
func removeOtherApexNS(dc *models.DomainConfig) {
	newList := make([]*models.RecordConfig, 0, len(dc.Records))
	for _, rec := range dc.Records {
		if rec.Type == "NS" {
			// apex NS inside dnsimple are expected.
			// We ignore them, warning as needed.
			// Child delegations are supported so we allow non-apex NS records.
			if rec.GetLabelFQDN() == dc.Name {
				if !isDnsimpleNameServerDomain(rec.GetTargetField()) {
					printer.Printf("Warning: dnsimple.com does not allow NS records to be modified. %s will not be added.\n", rec.GetTargetField())
				}
				continue
			}
		}
		newList = append(newList, rec)
	}
	dc.Records = newList
}

// Returns the correct combined content for all special record types, Target for everything else
// Using RecordConfig.GetTargetCombined returns priority in the string, which we do not allow
func getTargetRecordContent(rc *models.RecordConfig) string {
	switch rtype := rc.Type; rtype {
	case "CAA":
		return rc.GetTargetCombined()
	case "DS":
		return fmt.Sprintf("%d %d %d %s", rc.DsKeyTag, rc.DsAlgorithm, rc.DsDigestType, rc.DsDigest)
	case "NAPTR":
		return fmt.Sprintf(`%d %d "%s" "%s" "%s" %s`,
			rc.NaptrOrder, rc.NaptrPreference, rc.NaptrFlags, rc.NaptrService, rc.NaptrRegexp,
			rc.GetTargetField())
	case "SSHFP":
		return fmt.Sprintf("%d %d %s", rc.SshfpAlgorithm, rc.SshfpFingerprint, rc.GetTargetField())
	case "SRV":
		return fmt.Sprintf("%d %d %s", rc.SrvWeight, rc.SrvPort, rc.GetTargetField())
	case "TXT":
		return rc.GetTargetCombinedFunc(txtutil.EncodeQuoted)
	default:
		return rc.GetTargetField()
	}
}

// Returns the correct priority for the record type, 0 for records without priority
func getTargetRecordPriority(rc *models.RecordConfig) int {
	switch rtype := rc.Type; rtype {
	case "MX":
		return int(rc.MxPreference)
	case "SRV":
		return int(rc.SrvPriority)
	case "NAPTR":
		// Neither order nor preference
		return 0
	default:
		return 0
	}
}

// Compile the error messages returned by DNSimple's API into a single error message
func compileAttributeErrors(err *dnsimpleapi.ErrorResponse) error {
	message := fmt.Sprintf("%d %s", err.HTTPResponse.StatusCode, err.Message)
	for field, errors := range err.AttributeErrors {
		e := strings.Join(errors, "& ")
		message += fmt.Sprintf(": %s %s", field, e)
	}
	return errors.New(message)
}

// Return true if the string ends in one of DNSimple's name server domains
// False if anything else
func isDnsimpleNameServerDomain(name string) bool {
	for _, i := range nameServerSuffixes {
		if strings.HasSuffix(name, i) {
			return true
		}
	}
	return false
}

// Tests if the content is encoded, performing a naive check on the presence of quotes
// at the beginning and end of the string.
//
// This is a backward-compatible function to facilitate the TXT transition.
func isQuotedTXT(content string) bool {
	return content[0:1] == `"` && content[len(content)-1:] == `"`
}
