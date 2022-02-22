package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"strings"

	"cloud.google.com/go/compute/metadata"
	"github.com/PuerkitoBio/rehttp"
	"golang.org/x/oauth2/google"
	"google.golang.org/api/compute/v1"
	"google.golang.org/api/option"
	"gopkg.in/alecthomas/kingpin.v2"

	"google.golang.org/api/cloudresourcemanager/v1"
)

var (
	from = kingpin.Flag(
		"from", "The environment to compare from",
	).Envar("GCP_QUOTA_COMPARER_FROM").Required().String()

	to = kingpin.Flag(
		"to", "The environment to compare to",
	).Envar("GCP_QUOTA_COMPARER_TO").Required().String()

	regexFrom = kingpin.Flag(
		"regex.from", "The regex to use to match against the source projects.",
	).Envar("GCP_QUOTA_COMPARER_REGEX_FROM").Default(`prj-\w+-(?P<Name>.*)-[a-zA-Z0-9]{4}$`).String()

	regexTo = kingpin.Flag(
		"regex.to", "The regex to use to match against the target project.",
	).Envar("GCP_QUOTA_COMPARER_REGEX_TO").Default(`prj-\w+-%s-[a-zA-Z0-9]{4}$`).String()

	gcpMaxRetries = kingpin.Flag(
		"gcp.max-retries", "Max number of retries that should be attempted on 503 errors from gcp. ($GCP_EXPORTER_MAX_RETRIES)",
	).Envar("GCP_QUOTA_COMPARER_MAX_RETRIES").Default("0").Int()

	gcpHttpTimeout = kingpin.Flag(
		"gcp.http-timeout", "How long should gcp_exporter wait for a result from the Google API ($GCP_EXPORTER_HTTP_TIMEOUT)",
	).Envar("GCP_QUOTA_COMPARER_HTTP_TIMEOUT").Default("10s").Duration()

	gcpMaxBackoffDuration = kingpin.Flag(
		"gcp.max-backoff", "Max time between each request in an exp backoff scenario ($GCP_EXPORTER_MAX_BACKOFF_DURATION)",
	).Envar("GCP_QUOTA_COMPARER_MAX_BACKOFF_DURATION").Default("5s").Duration()

	gcpBackoffJitterBase = kingpin.Flag(
		"gcp.backoff-jitter", "The amount of jitter to introduce in a exp backoff scenario ($GCP_EXPORTER_BACKOFF_JITTER_BASE)",
	).Envar("GCP_QUOTA_COMPARER_BACKOFF_JITTER_BASE").Default("1s").Duration()

	gcpRetryStatuses = kingpin.Flag(
		"gcp.retry-statuses", "The HTTP statuses that should trigger a retry ($GCP_EXPORTER_RETRY_STATUSES)",
	).Envar("GCP_QUOTA_COMPARER_RETRY_STATUSES").Default("503").Ints()
)

type Quotas struct {
	projectId  string
	project    *compute.Project
	regionList *compute.RegionList
}

type Issue struct {
	fromProjectId string
	toProjectId   string
	region        string
	metric        string
	fromLimit     float64
	toLimit       float64
}

func GetQuotas(projectId string) (error, *Quotas) {
	// Create context and generate compute.Service
	ctx := context.Background()

	googleClient, err := google.DefaultClient(ctx, compute.ComputeReadonlyScope)
	if err != nil {
		return fmt.Errorf("Error creating Google client: %v", err), nil
	}

	googleClient.Timeout = *gcpHttpTimeout
	googleClient.Transport = rehttp.NewTransport(
		googleClient.Transport, // need to wrap DefaultClient transport
		rehttp.RetryAll(
			rehttp.RetryMaxRetries(*gcpMaxRetries),
			rehttp.RetryStatuses(*gcpRetryStatuses...)), // Cloud support suggests retrying on 503 errors
		rehttp.ExpJitterDelay(*gcpBackoffJitterBase, *gcpMaxBackoffDuration), // Set timeout to <10s as that is prom default timeout
	)

	computeService, err := compute.NewService(ctx, option.WithHTTPClient(googleClient))

	if err != nil {
		log.Fatalf("Failure when getting compute service: %v", err)
	}

	project, err := computeService.Projects.Get(projectId).Do()
	if err != nil {
		log.Printf("Failure when querying project quotas: %v", err)
		return nil, nil
	}

	regionList, err := computeService.Regions.List(projectId).Do()

	if err != nil {
		log.Printf("Failure when querying region quotas: %v", err)
		regionList = nil
	}

	return nil, &Quotas{
		projectId,
		project,
		regionList,
	}

}
func GetProjects(query string) ([]*cloudresourcemanager.Project, error) {
	// Create context and generate compute.Service
	ctx := context.Background()
	cloudresourcemanagerService, err := cloudresourcemanager.NewService(ctx)

	if err != nil {
		return nil, err
	}

	// needs to be active because we don't want to query inactive projects
	filter := strings.Join([]string{"lifecycleState:ACTIVE", query}, " ")

	log.Printf("Project filter: %v", filter)

	projectQuery := cloudresourcemanagerService.Projects.List().Filter(filter)

	response, err := projectQuery.Do()
	if err != nil {
		return nil, err
	}

	projects := response.Projects

	// log.Printf("Retrieved project list: %v", projects)

	return projects, nil
}

func GetProjectIds(p []*cloudresourcemanager.Project) []string {
	var list []string
	for _, project := range p {
		list = append(list, project.ProjectId)
	}
	return list
}

func GetProjectIdFromMetadata() (string, error) {
	client := metadata.NewClient(&http.Client{})

	project_id, err := client.ProjectID()
	if err != nil {
		return "", err
	}

	return project_id, nil
}

func main() {
	kingpin.Version("0.1.0")
	kingpin.Parse()

	fromProjects, err := GetProjects(*from)

	issues := []Issue{}

	if err != nil {
		log.Fatal(err)
	}

	toProjects, err := GetProjects(*to)
	if err != nil {
		log.Fatal(err)
	}

	for _, fromProject := range fromProjects {
		r := regexp.MustCompile(*regexFrom)
		projectNameMatch := r.FindStringSubmatch(fromProject.ProjectId)

		projectName := projectNameMatch[1]
		var toProjectFound *cloudresourcemanager.Project

		r1 := regexp.MustCompile(fmt.Sprintf(*regexTo, projectName))

		err, fromProjectQuotas := GetQuotas(fromProject.ProjectId)

		if err != nil {
			log.Fatalf("Error with project %s: %s", fromProject.ProjectId, err)
		}
		for _, toProject := range toProjects {
			toProjectId := toProject.ProjectId

			if r1.MatchString(toProjectId) {
				toProjectFound = toProject
				break
			}
		}

		if toProjectFound != nil {
			// log.Printf("Checking %s against %s...", fromProject.ProjectId, toProjectFound.ProjectId)
			err, toProjectQuotas := GetQuotas(toProjectFound.ProjectId)
			if err != nil {
				log.Fatalf("Error with project %s: %s", fromProject.ProjectId, err)
			}

			for _, fromProjectQuota := range fromProjectQuotas.project.Quotas {
				fromProjectQuotaMetric := fromProjectQuota.Metric
				fromProjectQuotaLimit := fromProjectQuota.Limit

				var toProjectQuota *compute.Quota = nil

				for i := range toProjectQuotas.project.Quotas {
					if toProjectQuotas.project.Quotas[i].Metric == fromProjectQuotaMetric {
						toProjectQuota = toProjectQuotas.project.Quotas[i]
						break
					}
				}

				if toProjectQuota == nil {
					log.Printf("[%s]: Metric %s does not exist", fromProject.ProjectId, fromProjectQuotaMetric)
					continue
				}

				toProjectQuotaMetric := toProjectQuota.Metric
				toProjectQuotaLimit := toProjectQuota.Limit

				if toProjectQuotaLimit != fromProjectQuotaLimit {
					log.Printf("[%s] [%s] (%f) limit differs from [%s] [%s] (%f)", fromProject.ProjectId, fromProjectQuotaMetric, fromProjectQuotaLimit, toProjectFound.ProjectId, toProjectQuotaMetric, toProjectQuotaLimit)
					issues = append(issues, Issue{
						fromProjectId: fromProject.Name,
						toProjectId:   toProjectFound.Name,
						metric:        fromProjectQuotaMetric,
						fromLimit:     fromProjectQuotaLimit,
						toLimit:       toProjectQuotaLimit,
					})
				}
			}

			// check regions
			for _, fromRegion := range fromProjectQuotas.regionList.Items {
				fromRegionName := fromRegion.Name
				var toRegionFound *compute.Region = nil
				for _, toRegion := range toProjectQuotas.regionList.Items {
					toRegionName := toRegion.Name
					if toRegionName == fromRegionName {
						toRegionFound = toRegion
						break
					}
				}

				if toRegionFound == nil {
					log.Printf("[%s]: Region %s does not exist", fromProject.ProjectId, fromRegionName)
					continue
				}

				// log.Printf("Checking %s/%s against %s/%s...", fromProject.ProjectId, fromRegionName, toProjectFound.ProjectId, toRegionFound.Name)

				for _, fromRegionQuota := range fromRegion.Quotas {
					fromRegionQuotaMetric := fromRegionQuota.Metric
					fromRegionQuotaLimit := fromRegionQuota.Limit

					var toRegionQuotaFound *compute.Quota = nil

					for i := range toRegionFound.Quotas {
						if toRegionFound.Quotas[i].Metric == fromRegionQuotaMetric {
							toRegionQuotaFound = toRegionFound.Quotas[i]
							break
						}
					}

					if toRegionQuotaFound == nil {
						log.Printf("[%s]/%s: Metric %s does not exist", fromProject.ProjectId, toRegionFound.Name, fromRegionQuotaMetric)
						continue
					}

					toRegionQuotaMetric := toRegionQuotaFound.Metric
					toRegionQuotaLimit := toRegionQuotaFound.Limit

					if toRegionQuotaLimit != fromRegionQuotaLimit {
						log.Printf("[%s/%s] [%s] (%f) limit differs from [%s/%s] [%s] (%f)", fromProject.ProjectId, fromRegionName, fromRegionQuotaMetric, fromRegionQuotaLimit, toProjectFound.ProjectId, toRegionFound.Name, toRegionQuotaMetric, toRegionQuotaLimit)
						issues = append(issues, Issue{
							fromProjectId: fromProject.Name,
							toProjectId:   toProjectFound.Name,
							region:        fromRegionName,
							metric:        fromRegionQuotaMetric,
							fromLimit:     fromRegionQuotaLimit,
							toLimit:       toRegionQuotaLimit,
						})
					}
				}
			}
		}
	}
}
