package ssmclient

import (
	"errors"
	"fmt"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/client"
	"github.com/aws/aws-sdk-go/service/ec2"
	"log"
	"net"
	"regexp"
	"strings"
)

var (
	// ErrInvalidTargetFormat is the error returned if the target format doesn't match the expected format
	// required by the resolver
	ErrInvalidTargetFormat = errors.New("invalid target format")
	// ErrNoInstanceFound is the error returned if a resolver was unable to find an instance
	ErrNoInstanceFound = errors.New("no instances returned from lookup")

	// RFC 1918 and 6598 address blocks
	privateNets = []net.IPNet{
		{IP: net.ParseIP("10.0.0.0"), Mask: net.IPv4Mask(0xff, 0, 0, 0)},       // 10.0/8
		{IP: net.ParseIP("172.16.0.0"), Mask: net.IPv4Mask(0xff, 0xf0, 0, 0)},  // 172.16/12
		{IP: net.ParseIP("192.168.0.0"), Mask: net.IPv4Mask(0xff, 0xff, 0, 0)}, // 192.168/16
		{IP: net.ParseIP("100.64.0.0"), Mask: net.IPv4Mask(0xff, 0xc0, 0, 0)},  // 100.64/10
	}
)

// TargetResolver is the interface specification for something which knows how to resolve and EC2 instance identifier
type TargetResolver interface {
	Resolve(string) (string, error)
}

// ResolveTarget attempts to find the instance ID of the target using a pre-defined resolution order.
// The first check will see if the target is already in the format of an EC2 instance ID.  Next, if
// the cfg parameter is not nil, checking by EC2 instance tags or private IPv4 IP address is performed.
// Finally, resolving by DNS TXT record will be attempted.
func ResolveTarget(target string, cfg client.ConfigProvider) (string, error) {
	resolvers := make([]TargetResolver, 0)

	if cfg != nil {
		resolvers = append(resolvers, NewTagResolver(cfg), NewIPResolver(cfg))
	}

	return ResolveTargetChain(strings.TrimSpace(target), append(resolvers, NewDNSResolver())...)
}

// ResolveTargetChain attempts to find the instance ID of the target using the provided list of TargetResolvers.
// The first check will always be to see if the target is already in the format of an EC2 instance ID before
// moving on to the resolution logic of the provided TargetResolvers.  If a resolver returns an error, the next
// resolver in the chain is checked.  If all resolvers fail to find an instance ID an error is returned.
func ResolveTargetChain(target string, resolvers ...TargetResolver) (inst string, err error) {
	var matched bool
	matched, err = regexp.MatchString(`^i-[[:xdigit:]]{8,}$`, target)
	if err != nil {
		return "", err
	}

	if matched {
		return target, nil
	}

	for _, res := range resolvers {
		inst, err = res.Resolve(target)
		if err != nil {
			continue
		}
		return inst, nil
	}
	return "", ErrNoInstanceFound
}

// NewTagResolver is a TargetResolver which knows how to find an EC2 instance using tags.
func NewTagResolver(cfg client.ConfigProvider) *tagResolver {
	return &tagResolver{&ec2Resolver{cfg: cfg}}
}

// NewIPResolver is a TargetResolver which knows how to find an EC2 instance using the private IPv4 address
func NewIPResolver(cfg client.ConfigProvider) *ipResolver {
	return &ipResolver{&ec2Resolver{cfg: cfg}}
}

// NewDNSResolver is a TargetResolver which knows how to find an EC2 instance using DNS TXT record lookups
func NewDNSResolver() *dnsResolver {
	return new(dnsResolver)
}

/*
 * DNS Resolver attempts to find an instance using a DNS TXT record lookup.  The DNS record is expected
 * to resolve to the EC2 instance ID associated with the DNS name.  If the DNS record is not found, or if
 * there is nothing which looks like an EC2 instance ID in the record data, and error is returned.
 */
type dnsResolver bool

func (r *dnsResolver) Resolve(target string) (string, error) {
	rr, err := net.LookupTXT(strings.TrimSpace(target))
	if err != nil {
		return "", err
	}

	var matched bool
	for _, rec := range rr {
		matched, err = regexp.MatchString(`^i-[[:xdigit:]]{8,}$`, rec)
		if err != nil {
			continue
		}

		if matched {
			return rec, nil
		}
	}

	return "", ErrNoInstanceFound
}

/*
 *  Tag Resolver attempts to find an instance using instance tags.  The expected format is tag_key:tag_value
 *  (ex. hostname:web0).  If the target to resolve doesn't look like a a colon-separated tag key:value pair,
 *  or no instance is found, an error is returned.  At most, 1 instance ID is returned; if more than 1 match
 *  is found, only the 1st element of the instances list is returned.  The nature of the AWS EC2 API will not
 *  guarantee ordering of the instances list.
 */
type tagResolver struct {
	*ec2Resolver
}

func (r *tagResolver) Resolve(target string) (string, error) {
	spec := strings.SplitN(strings.TrimSpace(target), `:`, 2)
	if len(spec) < 2 {
		return "", ErrInvalidTargetFormat
	}

	f := new(ec2.Filter).SetName(fmt.Sprintf(`tag:%s`, spec[0])).SetValues(aws.StringSlice([]string{spec[1]}))
	return r.ec2Resolver.Resolve(f)
}

/*
 *  IP Resolver attempts to find an instance by its private or public IPv4 address using the EC2 API.
 *  If the target doesn't look like an IPv4 address, a DNS lookup is tried. If neither of those produce
 *  an IPv4 address, or the EC2 instance lookup fails to find an instance, an error is returned.  At most,
 *  1 instance ID is returned; if more than 1 match is found, only the 1st element of the instances list
 *  is returned.  The nature of the AWS EC2 API will not guarantee ordering of the instances list.
 */
type ipResolver struct {
	*ec2Resolver
}

func (r *ipResolver) Resolve(target string) (string, error) {
	var pubIp, privIp []string
	var targets []net.IP

	trimmed := strings.TrimSpace(target)
	ip := net.ParseIP(trimmed)
	targets = []net.IP{ip}

	if ip == nil {
		// didn't look like an IP address, attempt DNS resolution ... maybe we'll find something there
		if addrs, err := net.LookupIP(trimmed); err == nil {
			targets = addrs
		} else {
			return "", ErrInvalidTargetFormat
		}
	}

	for _, t := range targets {
		// enforces that address is IPv4 or IPv6 address which can be represented as IPv4
		if v := t.To4(); v != nil {
			if isPrivateAddr(v) {
				privIp = append(privIp, v.String())
			} else {
				pubIp = append(pubIp, v.String())
			}
		}
	}

	// must resolve at least 1 public or private IPv4 address
	if len(pubIp) < 1 && len(privIp) < 1 {
		return "", ErrInvalidTargetFormat
	}

	// prefer any public address on the instance since it's entirely possible that there may be VPCs with overlapping
	// private IP space in an account and our DescribeInstances call will match any instance with that address,
	// regardless of which VPC is resides in.  In cases where there is overlapping IP space, caller should use a more
	// specific method for finding the instance, like tags.
	f := new(ec2.Filter).SetName(`private-ip-address`).SetValues(aws.StringSlice(privIp))
	if len(pubIp) > 0 {
		f.SetName(`ip-address`).SetValues(aws.StringSlice(pubIp))
	}

	return r.ec2Resolver.Resolve(f)
}

func isPrivateAddr(addr net.IP) bool {
	for _, n := range privateNets {
		if n.Contains(addr) {
			return true
		}
	}
	return false
}

/*
 *  EC2 Resolver calls the EC2 DescribeInstances API with a provided filter, which will return at most 1
 *  instance ID. If more than 1 instance matches the filter, the 1st instance ID in the list is returned.
 */
type ec2Resolver struct {
	cfg client.ConfigProvider
}

func (r *ec2Resolver) Resolve(filter ...*ec2.Filter) (string, error) {
	o, err := ec2.New(r.cfg).DescribeInstances(new(ec2.DescribeInstancesInput).SetFilters(filter))
	if err != nil {
		return "", err
	}

	for _, res := range o.Reservations {
		if len(res.Instances) > 0 {
			if len(res.Instances) > 1 {
				log.Print("WARNING: more than 1 instance found, using 1st value")
			}

			return *res.Instances[0].InstanceId, nil
		}
	}

	return "", ErrNoInstanceFound
}