package dht

import (
	"context"
	"errors"
	"fmt"
	"time"
	"sync"
	"github.com/ipfs/go-ipfs/pin"

	u "gx/ipfs/QmNiJuT8Ja3hMVpBHXv3Q6dwmperaQ6JjLtpMQgMCD7xvx/go-ipfs-util"
	recpb "gx/ipfs/QmUpttFinNDmNPgFwKN8sZK6BUtBmA68Y4KdSBDXa8t9sJ/go-libp2p-record/pb"
	ds "gx/ipfs/QmXRKBQA4wXP7xWbFiZsR1GP4HV6wMDQ1aWFxZZ4uBcPX9/go-datastore"
	pstore "gx/ipfs/QmXauCuJzmzapetmC6W4TuDJLL1yFFrVzSHoWv8YdbmnxH/go-libp2p-peerstore"
	pb "gx/ipfs/QmY1y2M1aCcVhy8UuTbZJBvuFbegZm47f9cDAdgxiehQfx/go-libp2p-kad-dht/pb"
	proto "gx/ipfs/QmZ4Qi3GaRbjcx28Sme5eMH7RQjGkt8wHxt2a65oLaeFEV/gogo-protobuf/proto"
	peer "gx/ipfs/QmZoWKhxUmZ2seW4BzX6fJkNR8hh9PsGModr7q171yq2SS/go-libp2p-peer"
	cid "gx/ipfs/QmcZfnkapfECQGcLZaf9B79NRg7cRa9EnZh4LSbkCzwNvY/go-cid"
	lgbl "gx/ipfs/Qmf9JgVLz46pxPXwG2eWSJpkqVCcjD4rp7zCRi2KP6GTNB/go-libp2p-loggables"
	base32 "gx/ipfs/QmfVj3x4D6Jkq9SEoi5n2NmoUomLwoeiwnYz2KQa15wRw6/base32"
)

// The number of closer peers to send on requests.
var CloserPeerCount = KValue

// dhthandler specifies the signature of functions that handle DHT messages.
type dhtHandler func(context.Context, peer.ID, *pb.Message) (*pb.Message, error)

func (dht *IpfsDHT) handlerForMsgType(t pb.Message_MessageType) dhtHandler {
	switch t {
	case pb.Message_GET_VALUE:
		return dht.handleGetValue
	case pb.Message_PUT_VALUE:
		return dht.handlePutValue
	case pb.Message_FIND_NODE:
		return dht.handleFindPeer
	case pb.Message_ADD_PROVIDER:
		return dht.handleAddProvider
	case pb.Message_GET_PROVIDERS:
		return dht.handleGetProviders
	case pb.Message_PING:
		return dht.handlePing

	case pb.Message_ADD_FILE:// TODO: sandy modified
		return dht.handleAddFile
	case pb.Message_REMOVE_FILE:
		return dht.handlePing
	default:
		return nil
	}
}

func (dht *IpfsDHT) handleGetValue(ctx context.Context, p peer.ID, pmes *pb.Message) (_ *pb.Message, err error) {
	eip := log.EventBegin(ctx, "handleGetValue", p)
	defer func() {
		if err != nil {
			eip.SetError(err)
		}
		eip.Done()
	}()
	log.Debugf("%s handleGetValue for key: %s", dht.self, pmes.GetKey())

	// setup response
	resp := pb.NewMessage(pmes.GetType(), pmes.GetKey(), pmes.GetClusterLevel())

	// first, is there even a key?
	k := pmes.GetKey()
	if k == "" {
		return nil, errors.New("handleGetValue but no key was provided")
		// TODO: send back an error response? could be bad, but the other node's hanging.
	}

	rec, err := dht.checkLocalDatastore(k)
	if err != nil {
		return nil, err
	}
	resp.Record = rec

	// Find closest peer on given cluster to desired key and reply with that info
	closer := dht.betterPeersToQuery(pmes, p, CloserPeerCount)
	if len(closer) > 0 {
		closerinfos := pstore.PeerInfos(dht.peerstore, closer)
		for _, pi := range closerinfos {
			log.Debugf("handleGetValue returning closer peer: '%s'", pi.ID)
			if len(pi.Addrs) < 1 {
				log.Warningf(`no addresses on peer being sent!
					[local:%s]
					[sending:%s]
					[remote:%s]`, dht.self, pi.ID, p)
			}
		}

		resp.CloserPeers = pb.PeerInfosToPBPeers(dht.host.Network(), closerinfos)
	}

	return resp, nil
}

func (dht *IpfsDHT) checkLocalDatastore(k string) (*recpb.Record, error) {
	log.Debugf("%s handleGetValue looking into ds", dht.self)
	dskey := convertToDsKey(k)
	iVal, err := dht.datastore.Get(dskey)
	log.Debugf("%s handleGetValue looking into ds GOT %v", dht.self, iVal)

	if err == ds.ErrNotFound {
		return nil, nil
	}

	// if we got an unexpected error, bail.
	if err != nil {
		return nil, err
	}

	// if we have the value, send it back
	log.Debugf("%s handleGetValue success!", dht.self)

	byts, ok := iVal.([]byte)
	if !ok {
		return nil, fmt.Errorf("datastore had non byte-slice value for %v", dskey)
	}

	rec := new(recpb.Record)
	err = proto.Unmarshal(byts, rec)
	if err != nil {
		log.Debug("failed to unmarshal DHT record from datastore")
		return nil, err
	}

	// if its our record, dont bother checking the times on it
	if peer.ID(rec.GetAuthor()) == dht.self {
		return rec, nil
	}

	var recordIsBad bool
	recvtime, err := u.ParseRFC3339(rec.GetTimeReceived())
	if err != nil {
		log.Info("either no receive time set on record, or it was invalid: ", err)
		recordIsBad = true
	}

	if time.Now().Sub(recvtime) > MaxRecordAge {
		log.Debug("old record found, tossing.")
		recordIsBad = true
	}

	// NOTE: We do not verify the record here beyond checking these timestamps.
	// we put the burden of checking the records on the requester as checking a record
	// may be computationally expensive

	if recordIsBad {
		err := dht.datastore.Delete(dskey)
		if err != nil {
			log.Error("Failed to delete bad record from datastore: ", err)
		}

		return nil, nil // can treat this as not having the record at all
	}

	return rec, nil
}

// Cleans the record (to avoid storing arbitrary data).
func cleanRecord(rec *recpb.Record) {
	rec.XXX_unrecognized = nil
	rec.TimeReceived = nil

	// Only include the author if there's a signature (otherwise, it's
	// unvalidated and could be anything).
	if len(rec.Signature) == 0 {
		rec.Author = nil
	}
}

// Store a value in this peer local storage
func (dht *IpfsDHT) handlePutValue(ctx context.Context, p peer.ID, pmes *pb.Message) (_ *pb.Message, err error) {
	eip := log.EventBegin(ctx, "handlePutValue", p)
	defer func() {
		if err != nil {
			eip.SetError(err)
		}
		eip.Done()
	}()

	dskey := convertToDsKey(pmes.GetKey())

	rec := pmes.GetRecord()
	if rec == nil {
		log.Infof("Got nil record from: %s", p.Pretty())
		return nil, errors.New("nil record")
	}
	cleanRecord(rec)

	if err = dht.verifyRecordLocally(rec); err != nil {
		log.Warningf("Bad dht record in PUT from: %s. %s", peer.ID(pmes.GetRecord().GetAuthor()), err)
		return nil, err
	}

	// record the time we receive every record
	rec.TimeReceived = proto.String(u.FormatRFC3339(time.Now()))

	data, err := proto.Marshal(rec)
	if err != nil {
		return nil, err
	}

	err = dht.datastore.Put(dskey, data)
	log.Debugf("%s handlePutValue %v", dht.self, dskey)
	return pmes, err
}

func (dht *IpfsDHT) handlePing(_ context.Context, p peer.ID, pmes *pb.Message) (*pb.Message, error) {
	log.Debugf("%s Responding to ping from %s!\n", dht.self, p)
	return pmes, nil
}

func (dht *IpfsDHT) handleFindPeer(ctx context.Context, p peer.ID, pmes *pb.Message) (*pb.Message, error) {
	defer log.EventBegin(ctx, "handleFindPeer", p).Done()
	resp := pb.NewMessage(pmes.GetType(), "", pmes.GetClusterLevel())
	var closest []peer.ID

	// if looking for self... special case where we send it on CloserPeers.
	if peer.ID(pmes.GetKey()) == dht.self {
		closest = []peer.ID{dht.self}
	} else {
		closest = dht.betterPeersToQuery(pmes, p, CloserPeerCount)
	}

	if closest == nil {
		log.Infof("%s handleFindPeer %s: could not find anything.", dht.self, p)
		return resp, nil
	}

	closestinfos := pstore.PeerInfos(dht.peerstore, closest)
	// possibly an over-allocation but this array is temporary anyways.
	withAddresses := make([]pstore.PeerInfo, 0, len(closestinfos))
	for _, pi := range closestinfos {
		if len(pi.Addrs) > 0 {
			withAddresses = append(withAddresses, pi)
			log.Debugf("handleFindPeer: sending back '%s'", pi.ID)
		}
	}

	resp.CloserPeers = pb.PeerInfosToPBPeers(dht.host.Network(), withAddresses)
	return resp, nil
}

func (dht *IpfsDHT) handleGetProviders(ctx context.Context, p peer.ID, pmes *pb.Message) (*pb.Message, error) {
	lm := make(lgbl.DeferredMap)
	lm["peer"] = func() interface{} { return p.Pretty() }
	eip := log.EventBegin(ctx, "handleGetProviders", lm)
	defer eip.Done()

	resp := pb.NewMessage(pmes.GetType(), pmes.GetKey(), pmes.GetClusterLevel())
	c, err := cid.Cast([]byte(pmes.GetKey()))
	if err != nil {
		eip.SetError(err)
		return nil, err
	}

	lm["key"] = func() interface{} { return c.String() }

	// debug logging niceness.
	reqDesc := fmt.Sprintf("%s handleGetProviders(%s, %s): ", dht.self, p, c)
	log.Debugf("%s begin", reqDesc)
	defer log.Debugf("%s end", reqDesc)

	// check if we have this value, to add ourselves as provider.
	has, err := dht.datastore.Has(convertToDsKey(c.KeyString()))
	if err != nil && err != ds.ErrNotFound {
		log.Debugf("unexpected datastore error: %v\n", err)
		has = false
	}

	// setup providers
	providers := dht.providers.GetProviders(ctx, c)
	if has {
		providers = append(providers, dht.self)
		log.Debugf("%s have the value. added self as provider", reqDesc)
	}

	if providers != nil && len(providers) > 0 {
		infos := pstore.PeerInfos(dht.peerstore, providers)
		resp.ProviderPeers = pb.PeerInfosToPBPeers(dht.host.Network(), infos)
		log.Debugf("%s have %d providers: %s", reqDesc, len(providers), infos)
	}

	// Also send closer peers.
	closer := dht.betterPeersToQuery(pmes, p, CloserPeerCount)
	if closer != nil {
		infos := pstore.PeerInfos(dht.peerstore, closer)
		resp.CloserPeers = pb.PeerInfosToPBPeers(dht.host.Network(), infos)
		log.Debugf("%s have %d closer peers: %s", reqDesc, len(closer), infos)
	}

	return resp, nil
}

func (dht *IpfsDHT) handleAddProvider(ctx context.Context, p peer.ID, pmes *pb.Message) (*pb.Message, error) {
	lm := make(lgbl.DeferredMap)
	lm["peer"] = func() interface{} { return p.Pretty() }
	eip := log.EventBegin(ctx, "handleAddProvider", lm)
	defer eip.Done()

	c, err := cid.Cast([]byte(pmes.GetKey()))
	if err != nil {
		eip.SetError(err)
		return nil, err
	}

	lm["key"] = func() interface{} { return c.String() }

	log.Debugf("%s adding %s as a provider for '%s'\n", dht.self, p, c)

	// add provider should use the address given in the message
	pinfos := pb.PBPeersToPeerInfos(pmes.GetProviderPeers())
	for _, pi := range pinfos {
		if pi.ID != p {
			// we should ignore this provider reccord! not from originator.
			// (we chould sign them and check signature later...)
			log.Debugf("handleAddProvider received provider %s from %s. Ignore.", pi.ID, p)
			continue
		}

		if len(pi.Addrs) < 1 {
			log.Debugf("%s got no valid addresses for provider %s. Ignore.", dht.self, p)
			continue
		}

		log.Infof("received provider %s for %s (addrs: %s)", p, c, pi.Addrs)
		if pi.ID != dht.self { // dont add own addrs.
			// add the received addresses to our peerstore.
			dht.peerstore.AddAddrs(pi.ID, pi.Addrs, pstore.ProviderAddrTTL)
		}
		dht.providers.AddProvider(ctx, c, p)
	}

	return nil, nil
}


func (dht *IpfsDHT) handleAddFile(ctx context.Context, p peer.ID, pmes *pb.Message) (*pb.Message, error) {//TODO: sandy modified
	lm := make(lgbl.DeferredMap)
	lm["peer"] = func() interface{} { return p.Pretty() }
	eip := log.EventBegin(ctx, "handleAddFile", lm)
	defer eip.Done()

	c, err := cid.Cast([]byte(pmes.GetKey()))
	if err != nil {
		eip.SetError(err)
		return nil, err
	}

	lm["key"] = func() interface{} { return c.String() }



	// add provider should use the address given in the message
	pinfos := pb.PBPeersToPeerInfos(pmes.GetProviderPeers())
	for _, pi := range pinfos {
		if pi.ID != p {
			// we should ignore this provider reccord! not from originator.
			// (we chould sign them and check signature later...)
			log.Debugf("handleAddProvider received provider %s from %s. Ignore.", pi.ID, p)
			continue
		}

		if len(pi.Addrs) < 1 {
			log.Debugf("%s got no valid addresses for provider %s. Ignore.", dht.self, p)
			continue
		}

		log.Infof("received provider %s for %s (addrs: %s)", p, c, pi.Addrs)
		if pi.ID != dht.self { // dont add own addrs.
			// add the received addresses to our peerstore.
			dht.peerstore.AddAddrs(pi.ID, pi.Addrs, pstore.ProviderAddrTTL)
		}
		dht.providers.AddProvider(ctx, c, p)
		fmt.Printf("[!!!!]%s adding %s as a provider for '%s'\n", dht.self, p, c)
	}

	var curentLevel int =  int(pmes.GetClusterLevelRaw())
	if curentLevel < 10 {
		peers, err := dht.GetClosestSuperPeers(ctx, pmes.GetKey()) // super nodes
		if err != nil {
			return nil, err
		}


		pmes.SetClusterLevel(curentLevel + 10);
		wg := sync.WaitGroup{}
		for pp := range peers {
			wg.Add(1)
			go func(pp peer.ID) {
				defer wg.Done()
				fmt.Printf("[!!!!]AddFile-level up %d , send message to close SuperNode(%s, %s)\n", pmes.GetClusterLevelRaw() , c, pp)
				if pp == dht.self {
					// myself is the closed peer
					dht.SendToClosestLeaf(ctx, pmes)
				}else {
					err := dht.sendMessage(ctx, pp, pmes)
					if err != nil {
						log.Debug(err)
					}
				}

			}(pp)
		}
		wg.Wait()

	}else if (curentLevel <20) && (curentLevel> 10) {

		dht.SendToClosestLeaf(ctx, pmes)
	}else {
		// leaf node , start get key cmd , call pin add file
		fmt.Printf("[!!!!]leaf node %s AddFile-level final (%s, %s), calling pin.SetTask \n", dht.self , c, p)
		pin.SetTask(pmes.GetKey())
		resultstr := pin.GetTaskResult()
		var str string
		str =<- resultstr
		fmt.Println(str)
		if str == "OK" {
			pmes.SetClusterLevel(88)
			return pmes, nil
		}else {
			pmes.SetClusterLevel(99)
			return pmes, nil
		}
	}


	return nil, nil
}
// TODO: sandy modified
func (dht *IpfsDHT) SendToClosestLeaf(ctx context.Context,  pmes *pb.Message) (*pb.Message, error){

	peers, err := dht.GetClosestPeers(ctx, pmes.GetKey())  // leaf nodes
	if err != nil {

		return nil, err
	}

	var count = pmes.GetClusterLevelRaw()-10
	pmes.SetClusterLevel(50);

	c, err := cid.Cast([]byte(pmes.GetKey()))
	if err != nil {

		return nil, err
	}


	pinfos := pb.PBPeersToPeerInfos(pmes.GetProviderPeers())
	oristr := pinfos[0].ID
	fmt.Printf("[!!!!]AddFile-level provider = %s \n", oristr)
	for p := range peers {


			if(p == oristr){
				continue
			}

			fmt.Printf("[!!!!]AddFile-level up 50 , send add task to leaf peers(%s, %s)\n", c, p)
			resp, err := dht.sendRequest(ctx, p, pmes)
			//err := dht.sendMessage(ctx, p, pmes)
			if err!= nil {
				fmt.Printf("[!!!!]task to leaf peers(%s, %s) error %s \n", c, p, err)
				continue
			}
			if resp != nil {
				if resp.GetClusterLevelRaw() == 88 {
					count --
					fmt.Printf("[!!!!]task to leaf peers(%s, %s) success,  %d left \n", c, p, count)

					if count == 0 {
						return nil, nil
					}
				}

			}

	}

	return pmes, nil
}


func (dht *IpfsDHT) GetClosestSuperPeers(ctx context.Context, key string) (<-chan peer.ID, error) {
	out := make(chan peer.ID, KValue)
	defer close(out)
	out <- dht.self
	return out, nil
}


func convertToDsKey(s string) ds.Key {
	return ds.NewKey(base32.RawStdEncoding.EncodeToString([]byte(s)))
}
