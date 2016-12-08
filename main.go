package main

import (
	"bufio"
	"bytes"
	"flag"
	"fmt"
	"gopkg.in/redis.v5"
	"io/ioutil"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/pkg/api/v1"
	"k8s.io/client-go/rest"
	"log"
	"os"
	"strings"
)

const (
	RedisClusterName            = "name"
	RedisClusterIp              = "ip"
	RedisClusterPort            = "port"
	RedisClusterQuorum          = "quorum"
	RedisClusterDownAfterMs     = "down-after-milliseconds"
	RedisClusterFailoverTimeout = "failover-timeout"
	RedisClusterParallelSync    = "parallel-syncs"
	RedisKeyRole                = "role"
	RedisKeyMaster              = "master"
	RedisKeyInfoSecion          = "Replication"
)

const (
	keyRedisInstancePort                   = "redis.instance.port"
	keyRedisSentinelClusterName            = "redis.sentinel.cluster.name"
	keyRedisSentinelClusterQuorum          = "redis.sentinel.cluster.quorum"
	keyRedisSentinelClusterDownAfterMs     = "redis.sentinel.cluster.down.after.ms"
	keyRedisSentinelClusterParallelSync    = "redis.sentinel.cluster.parallel.syncs"
	keyRedisSentinelClusterFailoverTimeout = "redis.sentinel.cluster.failover.timeout"
)

var confFilePath = flag.String("pathToFile", os.Getenv("PATH_TO_CONFIG_FILE"), "(PATH_TO_CONFIG_FILE) - Path to config file")
var nameSpace = flag.String("ns", os.Getenv("NAMESPACE"), "(NAMESPACE) - Namespace to operate in")
var sentinelHostPort = flag.String("sentinelHostPort", os.Getenv("SENTINEL_HOST_PORT"), "(SENTINEL_HOST_PORT) - Host and Port of Sentinel to get config from at startup")
var minionLabel = flag.String("minionLabels", os.Getenv("MINION_LABELS"), "(MINION_LABELS) - Labels of minions to look for (key)=(value),(key)=(value)")

func main() {
	log.Println("Starting up redis sentinel config copy")
	flag.Parse()
	flag.VisitAll(func(f *flag.Flag) {
		log.Printf("Flag \n Name:\t\t%v \n Value:\t\t%v \n DefaultVal:\t%v \n------------------- \n", f.Name, f.Value, f.DefValue)
	})

	printFile("Raw File", *confFilePath)

	f, err := os.OpenFile(*confFilePath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		log.Fatal("Error on file open", err)
	}
	defer f.Close()

	c := redis.NewClient(&redis.Options{
		Addr:       *sentinelHostPort,
		MaxRetries: 10,
	})

	if err := c.Ping().Err(); err != nil {
		log.Println("No Ping --- not other Sentinel found!")
		discoverClusters(f)
		f.Close()
		printFile("Final File", *confFilePath)
		log.Println("File written from Cluster query... Let's go!")
		return
	}

	cmd := redis.NewSliceCmd("SENTINEL", "masters")
	c.Process(cmd)

	if err := cmd.Err(); err != nil {
		log.Fatalf("Getting Masters from Sentinel went wrong %s --- %+v", err, cmd)
	}

	for nr, v := range cmd.Val() {
		inter := v.([]interface{})
		values := map[string]string{}
		for i := 0; i < len(inter); i = i + 2 {
			values[inter[i].(string)] = inter[i+1].(string)
		}

		addToFile(f, nr, values)
	}
	printFile("Final File", *confFilePath)
	f.Close()

	log.Println("Sentinel config generated... Let's go!")
}

func addToFile(f *os.File, nr int, values map[string]string) {
	writeOrDie(f, fmt.Sprintln("# start master", nr, values[RedisClusterName]))
	writeOrDie(f, fmt.Sprintln("sentinel monitor", values[RedisClusterName], values[RedisClusterIp], values[RedisClusterPort], values[RedisClusterQuorum]))
	writeOrDie(f, fmt.Sprintln("sentinel down-after-milliseconds", values[RedisClusterName], values[RedisClusterDownAfterMs]))
	writeOrDie(f, fmt.Sprintln("sentinel failover-timeout", values[RedisClusterName], values[RedisClusterFailoverTimeout]))
	writeOrDie(f, fmt.Sprintln("sentinel parallel-syncs", values[RedisClusterName], values[RedisClusterParallelSync]))
	writeOrDie(f, fmt.Sprintln("# end master", nr))
}

func discoverClusters(f *os.File) {
	cl := getClient()
	pl, err := cl.Core().Pods(*nameSpace).List(v1.ListOptions{
		LabelSelector: *minionLabel,
	})
	if err != nil {
		log.Fatalln("Error on querying Kubernetes", err)
		return
	}
	nrOfClusters := 0
	for _, v := range pl.Items {
		podIp := v.Status.PodIP

		c := redis.NewClient(&redis.Options{
			Addr:       podIp + ":" + v.Annotations[keyRedisInstancePort],
			MaxRetries: 10,
		})

		ri := c.Info(RedisKeyInfoSecion)
		if ri.Err() != nil {
			log.Println("Error talking to Redis", v.Name, podIp, ri.Err())
			continue
		}

		b, err := ri.Bytes()
		if err != nil {
			log.Println("Error getting bytes from InfoCommand", err)
		}

		rawLines := bufio.NewScanner(bytes.NewReader(b))
		kv := map[string]string{}
		for rawLines.Scan() {
			t := rawLines.Text()
			log.Println("RAW line --", t)
			sl := strings.Split(t, ":")
			if len(sl) == 2 {
				kv[sl[0]] = sl[1]
			}
		}

		if kv[RedisKeyRole] == RedisKeyMaster {
			nrOfClusters++
			values := map[string]string{}
			values[RedisClusterName] = v.Annotations[keyRedisSentinelClusterName]
			values[RedisClusterIp] = v.Status.PodIP
			values[RedisClusterPort] = v.Annotations[keyRedisInstancePort]
			values[RedisClusterDownAfterMs] = v.Annotations[keyRedisSentinelClusterDownAfterMs]
			values[RedisClusterQuorum] = v.Annotations[keyRedisSentinelClusterQuorum]
			values[RedisClusterParallelSync] = v.Annotations[keyRedisSentinelClusterParallelSync]
			values[RedisClusterFailoverTimeout] = v.Annotations[keyRedisSentinelClusterFailoverTimeout]
			addToFile(f, nrOfClusters, values)
		}
	}
}

func getClient() *kubernetes.Clientset {
	config, err := rest.InClusterConfig()
	if err != nil {
		log.Fatal("Create InClusterConfig", err.Error())
	}

	// creates the clientset
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		log.Fatal("Create API client for Config", err.Error())
	}
	return clientset
}

func writeOrDie(f *os.File, s string) {
	_, err := f.WriteString(s)
	if err != nil {
		log.Fatalln("Err on write", err, s, f)
	}
}

func printFile(s string, filePath string) {
	content, err := ioutil.ReadFile(filePath)
	if err != nil {
		log.Fatal("File read problem", err)
	}
	log.Printf("%v - Config file:\n%s", s, string(content))
}
