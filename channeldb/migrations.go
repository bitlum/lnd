package channeldb

import (
	"bytes"
	"fmt"

	"github.com/coreos/bbolt"
)

// migrateNodeAndEdgeUpdateIndex is a migration function that will update the
// database from version 0 to version 1. In version 1, we add two new indexes
// (one for nodes and one for edges) to keep track of the last time a node or
// edge was updated on the network. These new indexes allow us to implement the
// new graph sync protocol added.
func migrateNodeAndEdgeUpdateIndex(tx *bolt.Tx) error {
	// First, we'll populating the node portion of the new index. Before we
	// can add new values to the index, we'll first create the new bucket
	// where these items will be housed.
	nodes, err := tx.CreateBucketIfNotExists(nodeBucket)
	if err != nil {
		return fmt.Errorf("unable to create node bucket: %v", err)
	}
	nodeUpdateIndex, err := nodes.CreateBucketIfNotExists(
		nodeUpdateIndexBucket,
	)
	if err != nil {
		return fmt.Errorf("unable to create node update index: %v", err)
	}

	log.Infof("Populating new node update index bucket")

	// Now that we know the bucket has been created, we'll iterate over the
	// entire node bucket so we can add the (updateTime || nodePub) key
	// into the node update index.
	err = nodes.ForEach(func(nodePub, nodeInfo []byte) error {
		if len(nodePub) != 33 {
			return nil
		}

		log.Tracef("Adding %x to node update index", nodePub)

		// The first 8 bytes of a node's serialize data is the update
		// time, so we can extract that without decoding the entire
		// structure.
		updateTime := nodeInfo[:8]

		// Now that we have the update time, we can construct the key
		// to insert into the index.
		var indexKey [8 + 33]byte
		copy(indexKey[:8], updateTime)
		copy(indexKey[8:], nodePub)

		return nodeUpdateIndex.Put(indexKey[:], nil)
	})
	if err != nil {
		return fmt.Errorf("unable to update node indexes: %v", err)
	}

	log.Infof("Populating new edge update index bucket")

	// With the set of nodes updated, we'll now update all edges to have a
	// corresponding entry in the edge update index.
	edges, err := tx.CreateBucketIfNotExists(edgeBucket)
	if err != nil {
		return fmt.Errorf("unable to create edge bucket: %v", err)
	}
	edgeUpdateIndex, err := edges.CreateBucketIfNotExists(
		edgeUpdateIndexBucket,
	)
	if err != nil {
		return fmt.Errorf("unable to create edge update index: %v", err)
	}

	// We'll now run through each edge policy in the database, and update
	// the index to ensure each edge has the proper record.
	err = edges.ForEach(func(edgeKey, edgePolicyBytes []byte) error {
		if len(edgeKey) != 41 {
			return nil
		}

		// Now that we know this is the proper record, we'll grab the
		// channel ID (last 8 bytes of the key), and then decode the
		// edge policy so we can access the update time.
		chanID := edgeKey[33:]
		edgePolicyReader := bytes.NewReader(edgePolicyBytes)

		edgePolicy, err := deserializeChanEdgePolicy(
			edgePolicyReader, nodes,
		)
		if err != nil {
			return err
		}

		log.Tracef("Adding chan_id=%v to edge update index",
			edgePolicy.ChannelID)

		// We'll now construct the index key using the channel ID, and
		// the last time it was updated: (updateTime || chanID).
		var indexKey [8 + 8]byte
		byteOrder.PutUint64(
			indexKey[:], uint64(edgePolicy.LastUpdate.Unix()),
		)
		copy(indexKey[8:], chanID)

		return edgeUpdateIndex.Put(indexKey[:], nil)
	})
	if err != nil {
		return fmt.Errorf("unable to update edge indexes: %v", err)
	}

	log.Infof("Migration to node and edge update indexes complete!")

	return nil
}

// migrateAddInvoiceWithChannelPoint updates invoice structure by adding
// new channel point field. This migration ensures that previously existed
// invoices will be filled with empty channel point, so that new serialisation
// function wouldn't fail.
func migrateAddInvoiceWithChannelPoint(tx *bolt.Tx) error {
	// For every outgoing payment, we deserialize it with old function and
	// serialise with new, so that when user would like to fetch outgoing
	// payments, new deserialization function wouldn't fail.
	paymentsBucket := tx.Bucket(paymentBucket)
	if paymentsBucket != nil {
		if err := paymentsBucket.ForEach(func(paymentKey,
		paymentData []byte) error {
			// If the value is nil, then we ignore it as it may be
			// a sub-bucket.
			if paymentData == nil {
				return nil
			}

			r := bytes.NewReader(paymentData)
			payment, err := deserializeOutgoingPayment(r, nodeAndEdgeUpdateIndexVersion)
			if err != nil {
				return err
			}

			var b bytes.Buffer
			if err := serializeOutgoingPayment(&b, payment,
				invoiceWithChannelPointVersion); err != nil {
				return err
			}

			log.Tracef("Update schema of outgoing payment("+
				"%v), added empty channel point in invoice", payment.PaymentPreimage)

			return paymentsBucket.Put(paymentKey, b.Bytes())
		}); err != nil {
			return err
		}
	}

	// For every invoice, we deserialize it with old function and serialise
	// with new, so that when user would like to fetch invoices,
	// new deserialization function wouldn't fail.
	invoiceBucket := tx.Bucket(invoiceBucket)
	if invoiceBucket != nil {
		// Iterate through the entire key space of the top-level
		// invoice bucket. If key with a non-nil value stores the next
		// invoice ID which maps to the corresponding invoice.
		if err := invoiceBucket.ForEach(func(invoiceKey, invoiceData []byte) error {
			if invoiceData == nil {
				return nil
			}

			invoiceReader := bytes.NewReader(invoiceData)
			invoice, err := deserializeInvoice(invoiceReader,
				nodeAndEdgeUpdateIndexVersion)
			if err != nil {
				return err
			}

			var b bytes.Buffer
			if err := serializeInvoice(&b, invoice,
				invoiceWithChannelPointVersion); err != nil {
				return err
			}

			return invoiceBucket.Put(invoiceKey, b.Bytes())
		}); err != nil {
			return err
		}
	}

	log.Infof("Migration to invoices with channel point field has completed!")

	return nil
}

// migrateAddTypeToForwardEvent migrates db to use forward event with type
// and fail code.
func migrateAddTypeToForwardEvent(tx *bolt.Tx) error {
	logBucket := tx.Bucket(forwardingLogBucket)
	if logBucket != nil {
		return logBucket.ForEach(func(logTime, logData []byte) error {
			var event ForwardingEvent
			r := bytes.NewReader(logData)
			err := decodeForwardingEvent(r, &event, forwardEventWithType-1)
			if err != nil {
				return err
			}

			// Set previous forwards as successful
			event.Type = SuccessForward

			// Encode with new version of database
			var eventBuf bytes.Buffer
			err = encodeForwardingEvent(&eventBuf, &event, forwardEventWithType)
			if err != nil {
				return err
			}

			err = logBucket.Put(logTime, eventBuf.Bytes())
			if err != nil {
				return err
			}
			return nil
		})
	}

	return nil
}
