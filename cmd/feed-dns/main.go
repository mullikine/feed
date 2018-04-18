package main

import (
	"flag"

	"time"

	log "github.com/sirupsen/logrus"
	"github.com/sky-uk/feed/controller"
	"github.com/sky-uk/feed/dns"
	"github.com/sky-uk/feed/dns/adapter"
	"github.com/sky-uk/feed/elb"
	"github.com/sky-uk/feed/k8s"
	"github.com/sky-uk/feed/util/cmd"
	"github.com/sky-uk/feed/util/metrics"
	"fmt"
	"github.com/sky-uk/feed/dns/cdns"
)

var (
	debug                      bool
	kubeconfig                 string
	resyncPeriod               time.Duration
	healthPort                 int
	albNames                   cmd.CommaSeparatedValues
	elbLabelValue              string
	elbRegion                  string
	r53HostedZone              string
	pushgatewayURL             string
	pushgatewayIntervalSeconds int
	pushgatewayLabels          cmd.KeyValues
	awsAPIRetries              int
	internalHostname           string
	externalHostname           string
	cnameTimeToLive            time.Duration
	dnsProvider                string
	cdnsHostedZone             string
	cdnsInstanceGroupPrefix    string
)

func init() {
	const (
		defaultResyncPeriod               = time.Minute * 15
		defaultHealthPort                 = 12082
		defaultElbRegion                  = "eu-west-1"
		defaultElbLabelValue              = ""
		defaultHostedZone                 = ""
		defaultPushgatewayIntervalSeconds = 60
		defaultAwsAPIRetries              = 5
		defaultCnameTTL                   = 5 * time.Minute
		defaultCdnsInstanceGroupPrefix    = ""
		defaultDnsProvider                = ""
	)

	flag.BoolVar(&debug, "debug", false,
		"Enable debug logging.")
	flag.StringVar(&kubeconfig, "kubeconfig", "",
		"Path to kubeconfig for connecting to the apiserver. Leave blank to connect inside a cluster.")
	flag.DurationVar(&resyncPeriod, "resync-period", defaultResyncPeriod,
		"Resync with the apiserver periodically to handle missed updates.")
	flag.IntVar(&healthPort, "health-port", defaultHealthPort,
		"Port for checking the health of the ingress controller.")
	flag.Var(&albNames, "alb-names",
		"Comma delimited list of ALB names to use for Route53 updates. Should only include a single ALB name per LB scheme.")
	flag.StringVar(&elbRegion, "elb-region", defaultElbRegion,
		"AWS region for ELBs.")
	flag.StringVar(&elbLabelValue, "elb-label-value", defaultElbLabelValue,
		"Alias to ELBs tagged with " + elb.ElbTag + "=value. Route53 entries will be created to these,"+
			"depending on the scheme.")
	flag.StringVar(&r53HostedZone, "r53-hosted-zone", defaultHostedZone,
		"Route53 hosted zone id to manage.")
	flag.StringVar(&pushgatewayURL, "pushgateway", "",
		"Prometheus pushgateway URL for pushing metrics. Leave blank to not push metrics.")
	flag.IntVar(&pushgatewayIntervalSeconds, "pushgateway-interval", defaultPushgatewayIntervalSeconds,
		"Interval in seconds for pushing metrics.")
	flag.Var(&pushgatewayLabels, "pushgateway-label",
		"A label=value pair to attach to metrics pushed to prometheus. Specify multiple times for multiple labels.")
	flag.IntVar(&awsAPIRetries, "aws-api-retries", defaultAwsAPIRetries,
		"Number of times a request to the AWS API is retried.")
	flag.StringVar(&internalHostname, "internal-hostname", "",
		"Hostname of the internal facing load-balancer. If specified, external-hostname must also be given.")
	flag.StringVar(&externalHostname, "external-hostname", "",
		"Hostname of the internet facing load-balancer. If specified, internal-hostname must also be given.")
	flag.DurationVar(&cnameTimeToLive, "cname-ttl", defaultCnameTTL,
		"Time-to-live of CNAME records")

	flag.StringVar(&cdnsHostedZone, "dns-provider", defaultDnsProvider,
		"DNS provider to use. Valid values are: aws,gcp.")
	flag.StringVar(&cdnsHostedZone, "cdns-hosted-zone", defaultHostedZone,
		"Cloud DNS hosted zone name to manage.")
	flag.StringVar(&cdnsInstanceGroupPrefix, "cdns-instance-group-prefix", defaultCdnsInstanceGroupPrefix,
		"Prefix used to retrieve the GCLBs instance groups.")
}

func main() {
	flag.Parse()

	cmd.ConfigureLogging(debug)
	cmd.ConfigureMetrics("feed-dns", pushgatewayLabels, pushgatewayURL, pushgatewayIntervalSeconds)

	client, err := k8s.New(kubeconfig, resyncPeriod)
	if err != nil {
		log.Fatalf("Unable to create k8s client: %v", err)
	}

	dnsUpdater, err := createFrontendUpdater()
	if err != nil {
		log.Fatalf("Unable to create dns updater: %v", err)
	}

	controller := controller.New(controller.Config{
		KubernetesClient: client,
		Updaters:         []controller.Updater{dnsUpdater},
	})

	cmd.AddHealthMetrics(controller, metrics.PrometheusDNSSubsystem)
	cmd.AddHealthPort(controller, healthPort)
	cmd.AddSignalHandler(controller)

	if err := controller.Start(); err != nil {
		log.Fatal("Error while starting controller: ", err)
	}

	select {}
}

func createFrontendUpdater() (controller.Updater, error) {
	var dnsAdapter adapter.FrontendAdapter
	var err error
	switch dnsProvider {
	case "aws":
		validateAwsConfig()
		if internalHostname != "" || externalHostname != "" {
			addressesWithScheme := make(map[string]string)
			if internalHostname != "" {
				addressesWithScheme["internal"] = internalHostname
			}

			if externalHostname != "" {
				addressesWithScheme["internet-facing"] = externalHostname
			}

			dnsAdapter = adapter.NewStaticHostnameAdapter(addressesWithScheme, cnameTimeToLive)
		} else {

			config := adapter.AWSAdapterConfig{
				Region:        elbRegion,
				HostedZoneID:  r53HostedZone,
				ELBLabelValue: elbLabelValue,
				ALBNames:      albNames,
			}
			dnsAdapter, err = adapter.NewAWSAdapter(&config)
			if err != nil {
				return nil, fmt.Errorf("unable to create aws adapater: %v", err)
			}
		}
		return dns.New(r53HostedZone, dnsAdapter, awsAPIRetries), nil

	case "gcp":
		validateCdnsConfig()
		config := cdns.Config{
			InstanceGroupPrefix: cdnsInstanceGroupPrefix,
			HostedZone:          cdnsHostedZone,
		}
		dnsAdapter, err = cdns.NewAdapter(config)
		if err != nil {
			return nil, fmt.Errorf("unable to create gcp adapater: %v", err)
		}
		return cdns.NewUpdater(config)
	default:
		return nil, fmt.Errorf("invalid dns-provider %q. Must specify a valid value: aws, gcp", dnsProvider)
	}
}

func validateAwsConfig() {
	if r53HostedZone == "" {
		log.Fatal("Must supply r53-hosted-zone")
	}

	if elbLabelValue == "" && len(albNames) == 0 && internalHostname == "" && externalHostname == "" {
		log.Fatal("Must specify at least one of alb-names, elb-label-value, internal-hostname or external-hostname")
	}
	if (internalHostname != "" || externalHostname != "") && (elbLabelValue != "" || len(albNames) > 0) {
		log.Fatal("Can't supply both ELB/ALB and non-ALB/ELB hostname. Choose one or the other.")
	}
}

func validateCdnsConfig() {
	if cdnsInstanceGroupPrefix == "" {
		log.Fatalf("Must supply the cdns-instance-group-prefix value.")
	}
	if cdnsHostedZone == "" {
		log.Fatalf("Must supply the cdns-hosted-zone name.")
	}
}
