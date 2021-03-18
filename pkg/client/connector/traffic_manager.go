package connector

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"os"
	"os/user"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pkg/errors"
	"google.golang.org/grpc"
	errors2 "k8s.io/apimachinery/pkg/api/errors"

	"github.com/datawire/dlib/dexec"
	"github.com/datawire/dlib/dgroup"
	"github.com/datawire/dlib/dlog"
	"github.com/datawire/dlib/dtime"
	rpc "github.com/telepresenceio/telepresence/rpc/v2/connector"
	"github.com/telepresenceio/telepresence/rpc/v2/manager"
	"github.com/telepresenceio/telepresence/v2/pkg/client"
	"github.com/telepresenceio/telepresence/v2/pkg/client/actions"
	"github.com/telepresenceio/telepresence/v2/pkg/client/cache"
)

// trafficManager is a handle to access the Traffic Manager in a
// cluster.
type trafficManager struct {
	*installer // installer is also a k8sCluster

	// local information
	env         client.Env
	installID   string // telepresence's install ID
	userAndHost string // "laptop-username@laptop-hostname"

	// manager client
	managerClient manager.ManagerClient
	managerErr    error     // if managerClient is nil, why it's nil
	startup       chan bool // gets closed when managerClient is fully initialized (or managerErr is set)

	sessionInfo *manager.SessionInfo // sessionInfo returned by the traffic-manager

	// sshPort is a local TCP port number that the userd uses internally that gets forwarded to
	// the SSH port on the manager Pod.
	//
	// FIXME(lukeshu): sshPort is exposed to the rest of the machine because we use separate
	// `kubectl port-forward` and `ssh -D` processes; it should go away by way of moving the
	// port-forwarding to happen in the userd process.
	sshPort int32

	// Map of desired mount points for intercepts
	mountPoints sync.Map
}

// newTrafficManager returns a TrafficManager resource for the given
// cluster if it has a Traffic Manager service.
func newTrafficManager(_ context.Context, env client.Env, cluster *k8sCluster, installID string) (*trafficManager, error) {
	userinfo, err := user.Current()
	if err != nil {
		return nil, errors.Wrap(err, "user.Current()")
	}
	host, err := os.Hostname()
	if err != nil {
		return nil, errors.Wrap(err, "os.Hostname()")
	}

	// Ensure that we have a traffic-manager to talk to.
	ti, err := newTrafficManagerInstaller(cluster)
	if err != nil {
		return nil, errors.Wrap(err, "new installer")
	}
	tm := &trafficManager{
		installer:   ti,
		env:         env,
		installID:   installID,
		startup:     make(chan bool),
		userAndHost: fmt.Sprintf("%s@%s", userinfo.Username, host),
	}

	return tm, nil
}

func (tm *trafficManager) waitUntilStarted(c context.Context) error {
	select {
	case <-c.Done():
		return client.CheckTimeout(c, &client.GetConfig(c).Timeouts.TrafficManagerConnect, nil)
	case <-tm.startup:
		return tm.managerErr
	}
}

func (tm *trafficManager) run(c context.Context) error {
	err := tm.ensureManager(c, tm.env)
	if err != nil {
		tm.managerErr = err
		close(tm.startup)
		return err
	}

	kpfArgs := []string{
		"--namespace",
		managerNamespace,
		"svc/traffic-manager",
		fmt.Sprintf(":%d", ManagerPortSSH),
		fmt.Sprintf(":%d", ManagerPortHTTP)}

	// Scan port-forward output and grab the dynamically allocated ports
	rxPortForward := regexp.MustCompile(`\AForwarding from \d+\.\d+\.\d+\.\d+:(\d+) -> (\d+)`)
	outputScanner := func(sc *bufio.Scanner) interface{} {
		var sshPort, apiPort string
		for sc.Scan() {
			if rxr := rxPortForward.FindStringSubmatch(sc.Text()); rxr != nil {
				toPort, _ := strconv.Atoi(rxr[2])
				if toPort == ManagerPortSSH {
					sshPort = rxr[1]
					dlog.Debugf(c, "traffic-manager ssh-port %s", sshPort)
				} else if toPort == ManagerPortHTTP {
					apiPort = rxr[1]
					dlog.Debugf(c, "traffic-manager api-port %s", apiPort)
				}
				if sshPort != "" && apiPort != "" {
					return []string{sshPort, apiPort}
				}
			}
		}
		return nil
	}

	return client.Retry(c, "svc/traffic-manager port-forward", func(c context.Context) error {
		return tm.portForwardAndThen(c, kpfArgs, outputScanner, tm.initGrpc)
	}, 2*time.Second, 6*time.Second)
}

func (tm *trafficManager) bearerToken(ctx context.Context) string {
	token, err := cache.LoadTokenFromUserCache(ctx)
	if err != nil {
		return ""
	}
	return token.AccessToken
}

func (tm *trafficManager) initGrpc(c context.Context, portsIf interface{}) (err error) {
	ports := portsIf.([]string)
	sshPort, _ := strconv.Atoi(ports[0])
	tm.sshPort = int32(sshPort)

	// First check. Establish connection
	tos := &client.GetConfig(c).Timeouts
	tc, cancel := context.WithTimeout(c, tos.TrafficManagerAPI)
	defer cancel()

	var conn *grpc.ClientConn
	conn, err = grpc.DialContext(tc, "127.0.0.1:"+ports[1],
		grpc.WithInsecure(),
		grpc.WithNoProxy(),
		grpc.WithBlock())
	if err != nil {
		err = client.CheckTimeout(tc, &tos.TrafficManagerAPI, err)
		tm.managerErr = err
		close(tm.startup)
		return err
	}

	mClient := manager.NewManagerClient(conn)
	si, err := mClient.ArriveAsClient(tc, &manager.ClientInfo{
		Name:        tm.userAndHost,
		InstallId:   tm.installID,
		Product:     "telepresence",
		Version:     client.Version(),
		BearerToken: tm.bearerToken(c),
	})

	if err != nil {
		err = client.CheckTimeout(tc, &tos.TrafficManagerAPI, fmt.Errorf("ArriveAsClient: %w", err))
		dlog.Error(c, err)
		conn.Close()
		tm.managerErr = err
		close(tm.startup)
		return err
	}
	tm.managerClient = mClient
	tm.sessionInfo = si

	g := dgroup.NewGroup(c, dgroup.GroupConfig{})
	g.Go("remain", tm.remain)
	g.Go("intercept-port-forward", tm.workerPortForwardIntercepts)
	close(tm.startup)
	return g.Wait()
}

func (tm *trafficManager) session() *manager.SessionInfo {
	return tm.sessionInfo
}

func (tm *trafficManager) workloadInfoSnapshot(ctx context.Context, rq *rpc.ListRequest) *rpc.WorkloadInfoSnapshot {
	var iMap map[string]*manager.InterceptInfo

	namespace := tm.actualNamespace(rq.Namespace)
	if namespace == "" {
		// namespace is not currently mapped
		return &rpc.WorkloadInfoSnapshot{}
	}

	if is, _ := actions.ListMyIntercepts(ctx, tm.managerClient, tm.session().SessionId); is != nil {
		iMap = make(map[string]*manager.InterceptInfo, len(is))
		for _, i := range is {
			if i.Spec.Namespace == namespace {
				iMap[i.Spec.Agent] = i
			}
		}
	} else {
		iMap = map[string]*manager.InterceptInfo{}
	}
	var aMap map[string]*manager.AgentInfo
	if as, _ := actions.ListAllAgents(ctx, tm.managerClient, tm.session().SessionId); as != nil {
		aMap = make(map[string]*manager.AgentInfo, len(as))
		for _, a := range as {
			if a.Namespace == namespace {
				aMap[a.Name] = a
			}
		}
	} else {
		aMap = map[string]*manager.AgentInfo{}
	}

	filter := rq.Filter
	workloadInfos := make([]*rpc.WorkloadInfo, 0)
	depNames, err := tm.deploymentNames(ctx, namespace)
	if err != nil {
		dlog.Error(ctx, err)
		return &rpc.WorkloadInfoSnapshot{}
	}
	for _, depName := range depNames {
		iCept, ok := iMap[depName]
		if !ok && filter <= rpc.ListRequest_INTERCEPTS {
			continue
		}
		agent, ok := aMap[depName]
		if !ok && filter <= rpc.ListRequest_INSTALLED_AGENTS {
			continue
		}
		reason := ""
		if agent == nil && iCept == nil {
			// Check if interceptable
			dep, err := tm.findDeployment(ctx, namespace, depName)
			if err != nil {
				// Removed from snapshot since the name slice was obtained
				if !errors2.IsNotFound(err) {
					dlog.Error(ctx, err)
				}
				continue
			}
			matchingSvcs := tm.findMatchingServices("", dep)
			if len(matchingSvcs) == 0 {
				if !ok && filter <= rpc.ListRequest_INTERCEPTABLE {
					continue
				}
				reason = "No service with matching selector"
			}
		}

		workloadInfos = append(workloadInfos, &rpc.WorkloadInfo{
			Name:                   depName,
			NotInterceptableReason: reason,
			AgentInfo:              agent,
			InterceptInfo:          iCept,
			WorkloadResourceType:   "Deployment",
		})
	}

	for localIntercept, localNs := range tm.localIntercepts {
		if localNs == namespace {
			workloadInfos = append(workloadInfos, &rpc.WorkloadInfo{InterceptInfo: &manager.InterceptInfo{
				Spec:              &manager.InterceptSpec{Name: localIntercept, Namespace: localNs},
				Disposition:       manager.InterceptDispositionType_ACTIVE,
				MechanismArgsDesc: "as local-only",
			}})
		}
	}

	return &rpc.WorkloadInfoSnapshot{Workloads: workloadInfos}
}

func (tm *trafficManager) remain(c context.Context) error {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-c.Done():
			_ = tm.clearIntercepts(context.Background())
			_, _ = tm.managerClient.Depart(context.Background(), tm.session())
			return nil
		case <-ticker.C:
			_, err := tm.managerClient.Remain(c, &manager.RemainRequest{
				Session:     tm.session(),
				BearerToken: tm.bearerToken(c),
			})
			if err != nil {
				if c.Err() != nil {
					err = nil
				}
				return err
			}
		}
	}
}

func (tm *trafficManager) setStatus(ctx context.Context, r *rpc.ConnectInfo) {
	if tm == nil {
		return
	}
	r.BridgeOk = tm.check(ctx)
	if tm.managerClient == nil {
		r.Intercepts = &manager.InterceptInfoSnapshot{}
		r.Agents = &manager.AgentInfoSnapshot{}
		if err := tm.managerErr; err != nil {
			r.ErrorText = err.Error()
		}
	} else {
		agents, _ := actions.ListAllAgents(ctx, tm.managerClient, tm.session().SessionId)
		intercepts, _ := actions.ListMyIntercepts(ctx, tm.managerClient, tm.session().SessionId)
		r.Agents = &manager.AgentInfoSnapshot{Agents: agents}
		r.Intercepts = &manager.InterceptInfoSnapshot{Intercepts: intercepts}
		r.SessionInfo = tm.session()
	}
}

// Given a slice of AgentInfo, this returns another slice of agents with one
// agent per namespace, name pair.
func getRepresentativeAgents(_ context.Context, agents []*manager.AgentInfo) []*manager.AgentInfo {
	type deployment struct {
		name, namespace string
	}
	deployments := map[deployment]bool{}
	var representativeAgents []*manager.AgentInfo
	for _, agent := range agents {
		dep := deployment{name: agent.Name, namespace: agent.Namespace}
		if !deployments[dep] {
			deployments[dep] = true
			representativeAgents = append(representativeAgents, agent)
		}
	}
	return representativeAgents
}

func (tm *trafficManager) uninstall(c context.Context, ur *rpc.UninstallRequest) (*rpc.UninstallResult, error) {
	result := &rpc.UninstallResult{}
	agents, _ := actions.ListAllAgents(c, tm.managerClient, tm.session().SessionId)

	// Since deployments can have more than one replica, we get a slice of agents
	// where the agent to deployment mapping is 1-to-1.  This is important
	// because in the ALL_AGENTS or default case, we could edit the same
	// deployment n times for n replicas, which could cause race conditions
	agents = getRepresentativeAgents(c, agents)

	_ = tm.clearIntercepts(c)
	switch ur.UninstallType {
	case rpc.UninstallRequest_UNSPECIFIED:
		return nil, errors.New("invalid uninstall request")
	case rpc.UninstallRequest_NAMED_AGENTS:
		var selectedAgents []*manager.AgentInfo
		for _, di := range ur.Agents {
			found := false
			namespace := tm.actualNamespace(ur.Namespace)
			if namespace != "" {
				for _, ai := range agents {
					if namespace == ai.Namespace && di == ai.Name {
						found = true
						selectedAgents = append(selectedAgents, ai)
						break
					}
				}
			}
			if !found {
				result.ErrorText = fmt.Sprintf("unable to find a deployment named %s.%s with an agent installed", di, namespace)
			}
		}
		agents = selectedAgents
		fallthrough
	case rpc.UninstallRequest_ALL_AGENTS:
		if len(agents) > 0 {
			if err := tm.removeManagerAndAgents(c, true, agents); err != nil {
				result.ErrorText = err.Error()
			}
		}
	default:
		// Cancel all communication with the manager
		if err := tm.removeManagerAndAgents(c, false, agents); err != nil {
			result.ErrorText = err.Error()
		}
	}
	return result, nil
}

// check checks the status of teleproxy bridge by doing the equivalent of
//  curl http://traffic-manager.svc:8022.
// Note there is no namespace specified, as we are checking for bridge status in the
// current namespace.
func (br *trafficManager) check(c context.Context) bool {
	if br == nil {
		return false
	}
	address := fmt.Sprintf("localhost:%d", br.sshPort)
	conn, err := net.DialTimeout("tcp", address, 15*time.Second)
	if err != nil {
		dlog.Errorf(c, "fail to establish tcp connection to %s: %v", address, err)
		return false
	}
	defer conn.Close()

	msg, _, err := bufio.NewReader(conn).ReadLine()
	if err != nil {
		dlog.Errorf(c, "tcp read: %v", err)
		return false
	}
	if !strings.Contains(string(msg), "SSH") {
		dlog.Errorf(c, "expected SSH prompt, got: %v", string(msg))
		return false
	}
	return true
}

// sshPortForward synchronously runs an `ssh` process with the given port-forward args.  It retries
// for all errors; it only returns when the context is canceled.  For that reason, it doesn't return
// an error; it just always retries.
func (tm *trafficManager) sshPortForward(ctx context.Context, pfArgs ...string) {
	// XXX: probably need some kind of keepalive check for ssh, first
	// curl after wakeup seems to trigger detection of death
	sshArgs := append(append([]string{"ssh"}, pfArgs...), []string{
		"-F", "none", // don't load the user's config file

		// connection settings
		"-C", // compression
		"-oConnectTimeout=10",
		"-oStrictHostKeyChecking=no",     // don't bother checking the host key...
		"-oUserKnownHostsFile=/dev/null", // and since we're not checking it, don't bother remembering it either

		// port-forward settings
		"-N", // no remote command; just connect and forward ports
		"-oExitOnForwardFailure=yes",

		// where to connect to
		"-p", strconv.Itoa(int(tm.sshPort)),
		"telepresence@localhost",
	}...)

	// Do NOT use client.Retry; this has slightly more domain-specific knowledge regarding the
	// backoff: We don't backoff if the process was "long-lived".  SSH connections just
	// sometimes die; we should retry those immediately; we only want to back off when it looks
	// like there's a problem *establishing* the connection.  So we use process-lifetime as a
	// proxy for whether a connection was established or not.
	backoff := 100 * time.Millisecond
	for ctx.Err() == nil {
		start := time.Now()
		err := dexec.CommandContext(ctx, sshArgs[0], sshArgs[1:]...).Run()
		lifetime := time.Since(start)

		if ctx.Err() == nil {
			if err == nil {
				err = errors.New("ssh process terminated successfully, but unexpectedly")
			}
			dlog.Errorf(ctx, "communicating with manager: %v", err)
			if lifetime >= 20*time.Second {
				backoff = 100 * time.Millisecond
				dtime.SleepWithContext(ctx, backoff)
			} else {
				dtime.SleepWithContext(ctx, backoff)
				backoff *= 2
				if backoff > 3*time.Second {
					backoff = 3 * time.Second
				}
			}
		}
	}
}
