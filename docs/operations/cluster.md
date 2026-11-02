# Running a cluster

A cluster is a set of Intrabin nodes that share one control plane (users, monitors, incidents,
history) through Raft and probe from several locations. Every node runs the same binary.

## Sizing
| Voters | Tolerated voter failures | Notes |
|---|---|---|
| 1 | 0 | No high availability (a single node, or one voter plus non-voting probes). |
| 2 | 0 | Losing either voter stops all changes. The UI warns about this layout. |
| **3** | **1** | Recommended. |
| 5 | 2 | For larger or more failure-prone deployments. |

Non-voters (new members, or nodes kept as probe-only locations) replicate data and probe, but do not
count toward quorum.

## Network
| Port | Purpose |
|---|---|
| 8080/tcp | UI + API on every node (put a TLS reverse proxy in front for remote access). |
| 7443/tcp | Cluster port: Raft and the peer API, mutual TLS with the cluster CA. Must be reachable between all nodes. Do not expose it publicly. |

Environment variables:

| Variable | Meaning |
|---|---|
| `INTRABIN_CLUSTER_LISTEN` | Bind address for the cluster port (default `:7443`). |
| `INTRABIN_CLUSTER_ADVERTISE` | `host:port` other nodes use to reach this node's cluster port. |
| `INTRABIN_PUBLIC_URL` | This node's UI URL; followers link users to the leader with it. |
| `INTRABIN_NODE_NAME`, `INTRABIN_NODE_LOCATION` | Display name and location label. Distinct locations count as independent confirmations (`min_locations`). |
| `INTRABIN_JOIN_TOKEN`, `INTRABIN_JOIN_DATA_ACTION` | Optional automation: join on first start. `export` or `discard` is required if the node already has data. |
| `INTRABIN_CLUSTER_INIT=true` | Optional automation: found a cluster on first start. |

## Create a cluster
1. Install the first node as usual and complete setup.
2. **Cluster / Nodes → Create a cluster**, enter the node's cluster address (for example
   `10.0.0.11:7443`). The node becomes the first voter and leader. Its current data becomes the
   cluster's initial state, and monitor secrets are re-encrypted with a new cluster data key.

## Add a node
1. On the leader: **Cluster / Nodes → New join token**. Tokens are single-use, expire (15 min–24 h)
   and can be revoked. Only a hash is stored; copy the token when it is shown.
2. On the new node:
   * **Fresh node**: on the first-run screen choose **Join an existing cluster instead**, enter the
     node's setup token (from its logs), the join token and the node's cluster address.
   * **Node with existing data**: sign in as owner, **Cluster / Nodes → Join an existing cluster**, and
     choose **Export a backup, then replace** (a `pre-join-backup-*.db` file is written to the data
     directory) or **Discard and replace**. Joining is refused without one of these choices.
3. The node joins as a **non-voter**: it verifies the cluster's CA against the fingerprint inside the
   token (no trust on first use), sends a certificate request, receives a node certificate and the
   wrapped data key, and starts replicating.
4. On the leader, wait until the member shows **Reachable** and its applied index has caught up, then
   **Promote to voter**. Promotion is refused if the node is not reachable both ways or is behind.

## Behaviour during failures
* **Leader loss**: the remaining voters elect a new leader (typically within 1–3 s). Evaluation
  continues from the replicated monitor state; incidents are not duplicated.
* **Returning node**: a former leader rejoins as a follower and catches up from the log.
* **Quorum loss / partition**: a node that cannot reach a majority of voters shows **No quorum**,
  refuses configuration changes (`503 no_quorum` or `409 node_read_only`), stops incident evaluation,
  and keeps probing; follower results are buffered in a durable outbox and shipped when the leader is
  reachable again. Quorum is never lowered automatically.
* **Followers' UI** shows node and cluster status (read-only) and links to the leader. Users can sign
  in on any node while the cluster has quorum.

## Backups
`intrabin backup -out file.db` on any node captures the replicated state plus that node's raw results.
Also keep each node's `master.key` (it unseals that node's copy of the cluster data key) and the
`cluster/` directory if you want to restore a node in place.

## Removing a node
**Cluster / Nodes → Remove** on the leader. The removed node keeps its data and certificate but its
peer requests are rejected because it is no longer a member. To reuse the machine, wipe its data
directory and join again with a new token.

## Not yet available
* Automated recovery after the **permanent** loss of a majority of voters (forcing a new
  configuration). Until then, restore from backup into a new single-node cluster and re-join nodes.
* Certificate revocation lists and automatic certificate renewal (node certificates are valid for one
  year; membership checks block removed nodes from the peer API).
* `reset-password` refuses to run on cluster members (a local change would diverge); another owner or
  admin can reset passwords in the UI.
