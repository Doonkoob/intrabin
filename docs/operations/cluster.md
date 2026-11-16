# Running a cluster

A cluster is a set of Intrabin nodes that share one control plane (users, monitors, incidents,
history) through Raft and probe from several locations. Every node runs the same binary.

Nodes are identified by a persistent UUID, which survives updates. The panel displays their public
URL or cluster address plus a short ID, so identical hostnames remain distinguishable. The member
list reports each node's running version from live heartbeats.

After joining, sign in on any member with the leader's accounts. Joining replaces the selected
local account data with replicated cluster accounts and waits until that state is available.
Password changes are replicated across the cluster; follower sessions are confirmed locally
before login returns success.

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
Every node has a **cluster address**: how the other nodes reach it. It takes one of two forms:

| Cluster address | Traffic goes through | Use when |
|---|---|---|
| The node's public URL, e.g. `https://intrabin.example.com` or `http://203.0.113.5:8080` | The normal web port (443, 80, 8080…), as a WebSocket tunnel to `/api/v1/cluster/tunnel` | The node is behind a reverse proxy or CDN, or you cannot open another port. **Default** when `INTRABIN_PUBLIC_URL` is set (the installer sets it). |
| `host:port` of the cluster port, e.g. `10.0.0.11:7443` | A dedicated TCP port (`INTRABIN_CLUSTER_LISTEN`, default `:7443`) | Private networks between the nodes. |

In both cases the nodes authenticate each other with mutual TLS 1.3 against the cluster CA, and that
TLS session runs **end to end inside the tunnel**: a reverse proxy or CDN that terminates the outer
HTTPS connection sees only encrypted bytes and cannot read or change cluster traffic. Raft is
leader-initiated, so the leader must be able to reach every member and every member the leader; the
forms can be mixed (a leader at `https://…` and workers at `http://<ip>:8080`).

Proxies and CDNs must allow WebSocket on the tunnel path: Caddy (installer's domain mode), Nginx with
`proxy_set_header Upgrade/Connection`, Cloudflare and ArvanCloud (enable WebSocket for the domain) all
work. Idle tunnels are kept open with WebSocket pings every 25 s.

| Port | Purpose |
|---|---|
| 8080/tcp (or 443 via the proxy) | UI + API on every node; also the cluster tunnel for URL cluster addresses. |
| 7443/tcp | Cluster port, only needed for `host:port` cluster addresses. Do not expose it publicly. |

Environment variables:

| Variable | Meaning |
|---|---|
| `INTRABIN_CLUSTER_LISTEN` | Bind address for the cluster port (default `:7443`). |
| `INTRABIN_CLUSTER_ADVERTISE` | Default cluster address of this node: its public URL or `host:port` of the cluster port. Defaults to `INTRABIN_PUBLIC_URL`. |
| `INTRABIN_PUBLIC_URL` | This node's UI URL; followers link users to the leader with it, and it is the default cluster address. |
| `INTRABIN_NODE_NAME`, `INTRABIN_NODE_LOCATION` | Display name and location label. Distinct locations count as independent confirmations (`min_locations`). |
| `INTRABIN_JOIN_TOKEN`, `INTRABIN_JOIN_DATA_ACTION` | Optional automation: join on first start. `export` or `discard` is required if the node already has data. |
| `INTRABIN_CLUSTER_INIT=true` | Optional automation: found a cluster on first start. |

## Create a cluster
1. Install the first node as usual and complete setup.
2. **Cluster / Nodes → Create a cluster**, enter the node's cluster address (for example
   `https://intrabin.example.com`, or `10.0.0.11:7443`). The node becomes the first voter and leader.
   Its current data becomes the cluster's initial state, and monitor secrets are re-encrypted with a
   new cluster data key.

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

## Change the leader, or turn the leader into a worker
Elections happen on their own when a leader fails; to move leadership on purpose:

* On the leader's own row, **Hand over leadership**: pick the new leader and keep **Then make this node
  a worker (non-voter)** checked to turn the current leader into a probe-only worker afterwards.
* On another member's row, **Make leader** does the same for that member.

A non-voter is promoted to a voter first (it must be reachable both ways and caught up). The current
leader then hands over through Raft's leadership transfer and, if asked, requests its own demotion
from the new leader. Your browser then shows this node as a follower with a link to the new leader.
To make the old leader a voter again later, use **Promote to voter** on the new leader.

## Change a node's cluster address
**Change address** on a member's row (on the leader; the leader's own address too). This is how an
existing cluster moves from the cluster port to public URLs: change each member to its
`https://…` / `http://<ip>:8080` address. Before saving, the leader connects to the new address and
checks that it presents that node's certificate, so a wrong address is refused instead of cutting the
member off. The member picks up its new address automatically.

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
