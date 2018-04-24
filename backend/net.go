package backend

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"io/ioutil"
	"log"
	"net"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/fntlnz/wirey/pkg/wireguard"
	"github.com/vishvananda/netlink"
)

type Peer struct {
	PublicKey []byte
	Endpoint  string
	IP        *net.IP
}

type Interface struct {
	Backend    Backend
	Name       string
	privateKey []byte
	LocalPeer  Peer
}

func NewInterface(b Backend, ifname string, endpoint string, ipaddr string, privateKeyPath string) (*Interface, error) {
	if len(strings.Split(endpoint, ":")) != 2 {
		return nil, fmt.Errorf("endpoint must be in format <ip>:<port>, like 192.168.1.3:3459")
	}

	if _, err := os.Stat(privateKeyPath); os.IsNotExist(err) {
		privKey, err := wireguard.Genkey()
		if err != nil {
			return nil, err
		}

		err = ioutil.WriteFile(privateKeyPath, privKey, 0600)
		if err != nil {
			return nil, fmt.Errorf("error writing private key file: %s", err.Error())
		}
	}

	privKey, err := ioutil.ReadFile(privateKeyPath)

	if err != nil {
		return nil, fmt.Errorf("error opening private key file: %s", err.Error())
	}

	pubKey, err := wireguard.ExtractPubKey(privKey)
	if err != nil {
		return nil, err
	}
	ipnet := net.ParseIP(ipaddr)
	return &Interface{
		Backend:    b,
		Name:       ifname,
		privateKey: privKey,
		LocalPeer: Peer{
			PublicKey: pubKey,
			IP:        &ipnet,
			Endpoint:  endpoint,
		},
	}, nil
}

func checkLinkAlreadyConnected(name string, peers []Peer, localPeer Peer) bool {
	link, err := netlink.LinkByName(name)
	if err != nil {
		return false
	}
	if link == nil {
		return false
	}

	for _, peer := range peers {
		if bytes.Equal(peer.PublicKey, localPeer.PublicKey) {
			// oh gosh, I have the interface but the link is down
			if link.Attrs().OperState != netlink.OperUp {
				// TODO(fntlnz): check here that the link type is wireguard?
				return false
			}
			// Well I am already connected
			return true
		}
	}
	return false
}

func extractPeersSHA(workingPeers []Peer) string {
	sort.Slice(workingPeers, func(i, j int) bool {
		comparison := bytes.Compare(workingPeers[i].PublicKey, workingPeers[j].PublicKey)
		if comparison > 0 {
			return true
		}
		return false
	})
	keys := ""
	for _, p := range workingPeers {
		keys = fmt.Sprintf("%s%s", keys, p.PublicKey)
	}

	h := sha256.New()
	h.Write([]byte(keys))

	return fmt.Sprintf("%x", h.Sum(nil))
}

func (i *Interface) addressAlreadyTaken() (bool, error) {
	peers, err := i.Backend.GetPeers(i.Name)
	if err != nil {
		return false, err
	}
	for _, p := range peers {
		if p.IP.Equal(*i.LocalPeer.IP) && !bytes.Equal(i.LocalPeer.PublicKey, p.PublicKey) {
			return true, nil
		}
	}
	return false, nil
}

func (i *Interface) Connect() error {
	taken, err := i.addressAlreadyTaken()

	if err != nil {
		return err
	}

	if taken {
		return fmt.Errorf("address already taken: %s", *i.LocalPeer.IP)
	}
	// Leave so I can recreate the peer on the distributed store
	i.Backend.Leave(i.Name, i.LocalPeer)

	// Join
	err = i.Backend.Join(i.Name, i.LocalPeer)

	if err != nil {
		return err
	}

	peersSHA := ""
	for {
		workingPeers, err := i.Backend.GetPeers(i.Name)
		if err != nil {
			return err
		}

		// We don't change anything if the peers remain the same
		newPeersSHA := extractPeersSHA(workingPeers)
		log.Printf("new peer sha: %s\n", newPeersSHA)
		if newPeersSHA == peersSHA {
			peersSHA = newPeersSHA
			time.Sleep(time.Second * 5)
			log.Printf("doing nothing")
			continue
		}
		peersSHA = newPeersSHA

		log.Println("delete old link")
		// delete any old link
		link, _ := netlink.LinkByName(i.Name)
		if link != nil {
			netlink.LinkDel(link)
		}

		// create the actual link
		wirelink := &netlink.GenericLink{
			LinkAttrs: netlink.LinkAttrs{
				Name: i.Name,
			},
			LinkType: "wireguard",
		}
		err = netlink.LinkAdd(wirelink)
		if err != nil {
			return fmt.Errorf("error adding the wireguard link: %s", err.Error())
		}

		// Add the actual address to the link
		addr, err := netlink.ParseAddr(fmt.Sprintf("%s/24", i.LocalPeer.IP.String()))
		if err != nil {
			return fmt.Errorf("error parsing the new ip address: %s", err.Error())
		}

		// Configure wireguard
		// TODO(fntlnz) how do we assign the external ip address?
		s := strings.Split(i.LocalPeer.Endpoint, ":")
		port, err := strconv.Atoi(s[1])
		if err != nil {
			return fmt.Errorf("error during port conversion to int: %s", err.Error())
		}
		conf := wireguard.Configuration{
			Interface: wireguard.Interface{
				ListenPort: port,
				PrivateKey: string(i.privateKey),
			},
			Peers: []wireguard.Peer{},
		}

		for _, p := range workingPeers {
			peer := wireguard.Peer{
				PublicKey:  string(p.PublicKey),
				AllowedIPs: "0.0.0.0/0", //TODO(fntlnz) this should compute the list comma separated
				Endpoint:   p.Endpoint,
			}
			conf.Peers = append(conf.Peers, peer)
		}

		_, err = wireguard.SetConf(i.Name, conf)

		if err != nil {
			return err
		}

		netlink.AddrAdd(wirelink, addr)

		// Up the link
		err = netlink.LinkSetUp(wirelink)
		if err != nil {
			return err
		}
	}

	return nil
}