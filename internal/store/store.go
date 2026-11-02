// Package store 定义持久化契约与领域值对象。
//
// 领域包只依赖这里的接口，不依赖 sqlite 实现，便于替换与单测打桩。
package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
)

// 常见错误，handler 据此映射成 404/409/410。
var (
	ErrNotFound = errors.New("not found")
	ErrConflict = errors.New("conflict")
	ErrGone     = errors.New("gone")
)

// OwnerPrimary 是实例主人的归属标识。
//
// 单主人部署下，所有能登录的凭证——设备配对产生的 client、宿主签发的
// 访问密钥——都属于同一个主人，因此带同一个 owner_id，算出的
// owner_hash 也相同：用密钥写的私有片段，配对的浏览器设备看得到，
// 反之亦然；轮换密钥不会让旧内容失联。
//
// 匿名创建（owner token）是唯一的例外，它没有身份，按 token 隔离。
const OwnerPrimary = "primary"

// OwnerHash 把归属凭证换算成库里存的哈希。
//
// 三种来源用前缀区分，避免同一字符串在三种语义下撞到一起：
//
//	o:<ownerID>   稳定身份（设备与访问密钥共用）
//	c:<clientID>  单台设备，只在没有 owner_id 时作为回退
//	t:<token>     匿名创建者持有的一次性凭证
//
// 只存哈希：数据库泄露不等于凭证泄露。
func OwnerHash(prefix, value string) string {
	sum := sha256.Sum256([]byte(prefix + ":" + value))
	return hex.EncodeToString(sum[:])
}

// OwnerHashForOwner 是跨 client 稳定的归属哈希。
func OwnerHashForOwner(ownerID string) string { return OwnerHash("o", ownerID) }

// OwnerHashForClient 是单台设备的归属哈希。
//
// 只在 client 没有 owner_id 时使用。0005 迁移之后所有 client 都有
// owner_id，这条路径只服务于迁移前遗留的数据计算。
func OwnerHashForClient(clientID string) string { return OwnerHash("c", clientID) }

// OwnerHashForToken 是匿名创建者凭证的归属哈希。
func OwnerHashForToken(token string) string { return OwnerHash("t", token) }

// Store 是全部仓储的入口。实现必须支持并发安全（内部串行化即可）。
type Store interface {
	// Paste 返回 pastebin 仓储。
	Paste() PasteStore
	// Feed 返回订阅源仓储。
	Feed() FeedStore
	// Entry 返回条目仓储。
	Entry() EntryStore
	// Blob 返回媒体元数据仓储。
	Blob() BlobStore
	// Media 返回待本地化资源的重试队列。
	Media() MediaStore
	// Auth 返回设备、会话、配对码仓储。
	Auth() AuthStore
	// Job 返回清理任务执行记录仓储。
	Job() JobStore

	// Ping 供 /readyz 使用。
	Ping(ctx context.Context) error
	// SchemaVersion 返回当前迁移版本。
	SchemaVersion(ctx context.Context) (int, error)
	// Stats 一次性汇总各表计数，供系统页展示。
	Stats(ctx context.Context) (Stats, error)
	// Close 关闭底层连接。调用后 Store 不可再用。
	Close() error
}

// PasteStore 是 pastebin 的持久化契约。
type PasteStore interface {
	// Create 插入一条片段。slug 冲突时返回 ErrConflict。
	Create(ctx context.Context, p *Paste) error
	// GetBySlug 按短链读取。不存在返回 ErrNotFound；
	// 是否过期由调用方判断（仓储层不解释业务时间）。
	GetBySlug(ctx context.Context, slug string) (*Paste, error)
	// Update 按 slug 覆盖可变字段。已软删的记录返回 ErrNotFound。
	Update(ctx context.Context, p *Paste) error
	// SoftDelete 置 deleted_at。对已删除的记录返回 ErrNotFound。
	SoftDelete(ctx context.Context, slug string) error
	// ClaimBurn 原子地把 burn_after_read 的片段标记为已读。
	// 返回 true 表示本次调用抢到了唯一一次读取权；false 表示已被别人读走。
	ClaimBurn(ctx context.Context, slug string, now int64) (bool, error)
	// List 按归属过滤并倒序分页。
	// ownerHash 始终参与过滤：空字符串匹配 owner_hash=''（迁移前的历史数据），
	// 而不是"返回全部"。归属过滤必须 fail-closed。
	List(ctx context.Context, ownerHash string, limit int, cursor string) ([]Paste, string, error)
	// CountByOwner 统计某归属下未删除的片段数。
	// 与 List 一样按 ownerHash 过滤，空字符串匹配历史数据。
	CountByOwner(ctx context.Context, ownerHash string) (int64, error)
	// PurgeExpired 物理删除过期或已软删超过保留期的记录，返回删除行数。
	PurgeExpired(ctx context.Context, now int64, softDeletedBefore int64) (int64, error)
}

// FeedStore 是订阅源的持久化契约。
type FeedStore interface {
	// Upsert 按 url 唯一键插入或更新。返回记录与是否为新建。
	Upsert(ctx context.Context, f *Feed) (Feed, bool, error)
	// Get 按主键读取。
	Get(ctx context.Context, id int64) (*Feed, error)
	// GetByURL 按订阅地址读取，用于推送去重。
	GetByURL(ctx context.Context, url string) (*Feed, error)
	// List 返回全部订阅，附带未读数。
	List(ctx context.Context) ([]Feed, error)
	// Update 覆盖调度与抓取状态字段。
	Update(ctx context.Context, f *Feed) error
	// Delete 删除订阅并级联清理条目、媒体记录与 blob 引用。
	// 引用计数靠显式删除 entry_blob 回退，不能只靠外键 CASCADE。
	Delete(ctx context.Context, id int64) error
	// Due 返回 next_fetch_at <= now 且启用的订阅，供调度器取用。
	Due(ctx context.Context, now int64, limit int) ([]Feed, error)
}

// EntryStore 是条目的持久化契约。
type EntryStore interface {
	// InsertIfNew 在 (feed_id, guid) 冲突时返回 existing=true 且不修改已有行。
	InsertIfNew(ctx context.Context, e *Entry) (bool, error)
	// Get 按主键读取，不含正文。
	Get(ctx context.Context, id int64) (*Entry, error)
	// List 按查询条件分页。列表不含 content_html，避免一页几十 MB。
	List(ctx context.Context, q EntryQuery) ([]Entry, string, error)
	// SetRead 置 read_at。read=true 写当前时间，false 置空。
	SetRead(ctx context.Context, id int64, read bool) error
	// SetStarred 置 starred_at，语义同 SetRead。
	SetStarred(ctx context.Context, id int64, starred bool) error
	// BulkSetRead 批量置已读状态。超过 200 条会被拒绝。
	BulkSetRead(ctx context.Context, ids []int64, read bool) error
	// UnreadCount 统计未读数。feedID 为 0 时统计全部。
	UnreadCount(ctx context.Context, feedID int64) (int64, error)
	// Purge 删除超过保留期且未标星的条目，返回删除行数。
	Purge(ctx context.Context, before int64, keepStarred bool) (int64, error)
}

// EntryQuery 是条目列表的过滤条件。零值字段表示不限。
type EntryQuery struct {
	FeedID  int64 // 0 表示不限
	Unread  bool
	Starred bool
	Search  string
	Limit   int
	Cursor  string
	// WithContent 决定返回的条目里是否保留 content_html / content_text。
	//
	// 默认 false：一页 50 条的正文可能有几十 MB，列表接口不需要。
	// 宿主读内容（GET /hub/entries）传 true——它就是要渲染正文。
	// 注意 SQL 始终会读这两列，这个开关只控制是否在返回前清空；
	// 它是带宽开关，不是查询优化开关。
	WithContent bool
}

// BlobStore 是媒体元数据的持久化契约。文件本身在 blob.Store 里。
type BlobStore interface {
	// Upsert 插入或更新引用，返回最终行。
	Upsert(ctx context.Context, b *Blob) error
	// Get 读取 blob 元数据。
	Get(ctx context.Context, sha string) (*Blob, error)
	// Link 建立 entry 与 blob 的关联；已存在时幂等。
	Link(ctx context.Context, entryID int64, sha string) error
	// Unreferenced 返回 ref_count = 0 且早于 before 的 blob。
	Unreferenced(ctx context.Context, before int64, limit int) ([]Blob, error)
	// Delete 删除 blob 行与全部 entry_blob 引用。
	Delete(ctx context.Context, sha string) error
	// TotalSize 汇总占用字节，用于配额校准。
	TotalSize(ctx context.Context) (int64, error)
	// Count 返回 blob 行数。
	Count(ctx context.Context) (int64, error)
}

// MediaRecord 是一条待本地化的资源。失败记录保留下来供重试任务使用。
type MediaRecord struct {
	ID        int64
	EntryID   int64
	SourceURL string
	SHA256    string
	Status    string
	Error     string
	Attempts  int
	UpdatedAt int64
}

// MediaStore 记录每条待本地化资源的下载状态，供重试任务使用。
type MediaStore interface {
	// Record 在 (entry_id, source_url) 上幂等写入。
	Record(ctx context.Context, r *MediaRecord) error
	// Retryable 返回仍需重试的记录：状态为 pending/failed、次数未用尽、
	// 且更新时间在窗口内。
	Retryable(ctx context.Context, maxAttempts int, after int64, limit int) ([]MediaRecord, error)
	// Update 覆盖一条媒体记录的状态。
	Update(ctx context.Context, r *MediaRecord) error
	// ByEntry 返回某条目的全部媒体记录。
	ByEntry(ctx context.Context, entryID int64) ([]MediaRecord, error)
	// SetEntryHTML 回写条目的正文，供媒体重试后就地修补占位图。
	SetEntryHTML(ctx context.Context, entryID int64, html, text string) error
}

// AuthStore 是设备、会话、配对码的持久化契约。
type AuthStore interface {
	// CreateClient 登记一台设备。id 冲突返回 ErrConflict。
	CreateClient(ctx context.Context, c *Client) error
	// GetClient 按 id 读取设备。
	GetClient(ctx context.Context, id string) (*Client, error)
	// ListClients 返回全部设备，含已撤销的。
	ListClients(ctx context.Context) ([]Client, error)
	// RevokeClient 撤销设备：置 revoked_at 并同时撤销其全部会话。
	RevokeClient(ctx context.Context, id string, now int64) error
	// TouchClient 更新最近活跃时间。
	TouchClient(ctx context.Context, id string, now int64) error

	// CreateChallenge 写入一次性 nonce。
	CreateChallenge(ctx context.Context, ch *Challenge) error
	// ConsumeChallenge 原子地标记已用；已用或过期返回 ErrNotFound。
	ConsumeChallenge(ctx context.Context, nonce string, now int64) (*Challenge, error)

	// CreateSession 写入会话。token 只存哈希。
	CreateSession(ctx context.Context, s *Session) error
	// GetSession 按 token 哈希读取会话。
	GetSession(ctx context.Context, tokenHash []byte) (*Session, error)
	// TouchSession 更新最近使用时间。
	TouchSession(ctx context.Context, tokenHash []byte, now int64) error
	// RevokeSession 撤销单个会话。
	RevokeSession(ctx context.Context, tokenHash []byte, now int64) error
	// RevokeClientSessions 撤销某设备的全部会话。
	RevokeClientSessions(ctx context.Context, clientID string, now int64) error
	// PurgeSessions 清理过期或已撤销的会话与挑战，返回删除行数。
	PurgeSessions(ctx context.Context, now int64) (int64, error)

	// CreateEnrollment 写入一次性配对码。只存哈希。
	CreateEnrollment(ctx context.Context, e *Enrollment) error
	// ConsumeEnrollment 原子地标记已用；已用或过期返回 ErrNotFound。
	ConsumeEnrollment(ctx context.Context, tokenHash []byte, now int64) (*Enrollment, error)
	// PurgeEnrollments 清理过期或已使用的配对码。
	PurgeEnrollments(ctx context.Context, now int64) (int64, error)

	// CreateAccessKey 写入一把宿主签发的长期密钥。只存哈希。
	CreateAccessKey(ctx context.Context, k *AccessKey) error
	// GetAccessKeyByHash 按明文哈希读取，用于兑换登录。
	// 不存在返回 ErrNotFound；已撤销或过期也返回 ErrNotFound。
	GetAccessKeyByHash(ctx context.Context, keyHash []byte, now int64) (*AccessKey, error)
	// ListAccessKeys 返回全部密钥（含已撤销的），不含明文。
	ListAccessKeys(ctx context.Context) ([]AccessKey, error)
	// TouchAccessKey 更新最近使用时间。
	TouchAccessKey(ctx context.Context, id string, now int64) error
	// RevokeAccessKey 撤销密钥并连带撤销它绑定的 client 及其全部会话。
	RevokeAccessKey(ctx context.Context, id string, now int64) error
}

// JobStore 记录清理任务的每次执行。
type JobStore interface {
	// Start 记录任务开始，返回 run id。
	Start(ctx context.Context, job string, at int64) (int64, error)
	// Finish 记录任务结束状态与摘要。
	Finish(ctx context.Context, id int64, status, detail string, at int64) error
	// Recent 返回最近的任务执行记录，倒序。
	Recent(ctx context.Context, limit int) ([]JobRun, error)
	// Purge 删除早于指定时间的执行记录。
	Purge(ctx context.Context, before int64) (int64, error)
}

// ---- 值对象 ----

// Paste 是一个文本片段。ExpiresAt 与 DeletedAt 为 nil 表示永久/未删除。
type Paste struct {
	ID            string
	Slug          string
	Title         string
	Body          string
	Language      string
	Visibility    string
	PasswordHash  string
	ExpiresAt     *int64
	BurnAfterRead bool
	SizeBytes     int64
	CreatedAt     int64
	UpdatedAt     int64
	DeletedAt     *int64
	// OwnerHash 是"持有即拥有"凭证的 sha256。
	// 登录用户用 client id，匿名用户用服务端生成的一次性 token。
	// 空字符串表示迁移前的历史数据。
	OwnerHash string
}

// Live 表示未删除且未过期。
func (p *Paste) Live(now int64) bool {
	if p.DeletedAt != nil {
		return false
	}
	return p.ExpiresAt == nil || *p.ExpiresAt > now
}

// Feed 是一个订阅源。下次抓取时刻由 NextFetchAt 驱动，由轮询器消费。
type Feed struct {
	ID      int64
	URL     string
	SiteURL string
	Title   string
	// Source 记录这条订阅从哪来：hub（Homehub 推送）或 manual（前端手动添加）。
	Source        string
	Enabled       bool
	FetchInterval int64
	Etag          string
	LastModified  string
	LastFetchAt   *int64
	NextFetchAt   *int64
	LastStatus    int
	LastError     string
	ErrorCount    int
	CreatedAt     int64
	UnreadCount   int64 // 列出时填充，非持久化字段
}

// Entry 是一个已本地化的条目。ContentHTML 是重写后的最终形态。
type Entry struct {
	ID          int64
	FeedID      int64
	GUID        string
	URL         string
	Title       string
	Author      string
	PublishedAt *int64
	FetchedAt   int64
	Summary     string
	ContentHTML string
	ContentText string
	ReadAt      *int64
	StarredAt   *int64
}

// Blob 是内容寻址的媒体元数据。RefCount 由 entry_blob 触发器维护。
type Blob struct {
	SHA256     string
	Size       int64
	MIME       string
	Ext        string
	Kind       string
	RefCount   int64
	CreatedAt  int64
	LastSeenAt int64
}

// Client 是一台已登记的设备。只存公钥，私钥从未离开客户端。
type Client struct {
	ID        string
	Name      string
	PublicKey []byte
	Algorithm string
	// OwnerID 是跨 client 稳定的归属标识。
	//
	// 空表示"这台设备自己就是一个归属"——设备配对产生的 client 就是这种，
	// 行为与引入该字段前完全一致。访问密钥产生的 client 会填同一个
	// 实例级标识，使轮换密钥后新旧密钥看到同一批片段。
	OwnerID    string
	CreatedAt  int64
	LastSeenAt *int64
	RevokedAt  *int64
}

// Active 表示设备未被撤销。
func (c *Client) Active() bool { return c.RevokedAt == nil }

// Challenge 只承载调用方需要的字段：是否已被消费由 ConsumeChallenge
// 的返回值表达，不需要在值对象里再带一个可能过期的快照。
type Challenge struct {
	Nonce     string
	ClientID  string
	CreatedAt int64
	ExpiresAt int64
}

// Session 是一次登录会话。TokenHash 是 refresh token 的 sha256。
type Session struct {
	TokenHash  []byte
	ClientID   string
	CreatedAt  int64
	ExpiresAt  int64
	LastUsedAt int64
	UserAgent  string
	RevokedAt  *int64
}

// Live 表示会话仍可用于换发 access token。
func (s *Session) Live(now int64) bool {
	return s.RevokedAt == nil && s.ExpiresAt > now
}

// Enrollment 同 Challenge：是否已用由 ConsumeEnrollment 的返回值表达。
type Enrollment struct {
	TokenHash []byte
	NameHint  string
	CreatedAt int64
	ExpiresAt int64
}

// AccessKey 是宿主签发的一把长期登录凭证。
//
// KeyHash 是 sha256(规范化后的明文)；明文只在签发响应里出现一次。
// ClientID 指向一台「无主客户端」：签发时生成了密钥对但私钥已丢弃，
// 所以这台 client 只能靠 AccessKey 兑换登录，无法走挑战-响应。
type AccessKey struct {
	ID         string
	Name       string
	KeyHash    []byte
	ClientID   string
	CreatedAt  int64
	ExpiresAt  *int64
	LastUsedAt *int64
	RevokedAt  *int64
}

// Live 表示这把密钥现在还能换到会话。
func (k *AccessKey) Live(now int64) bool {
	if k.RevokedAt != nil {
		return false
	}
	return k.ExpiresAt == nil || *k.ExpiresAt > now
}

// Stats 是系统页的汇总数据。单次查询比多次往返便宜。
type Stats struct {
	Pastes       int64 `json:"pastes"`
	Feeds        int64 `json:"feeds"`
	Entries      int64 `json:"entries"`
	Unread       int64 `json:"unread"`
	Blobs        int64 `json:"blobs"`
	BlobBytes    int64 `json:"blob_bytes"`
	PendingMedia int64 `json:"pending_media"`
}

// JobRun 是一次清理任务的执行记录。
type JobRun struct {
	ID         int64  `json:"id"`
	Job        string `json:"job"`
	StartedAt  int64  `json:"started_at"`
	FinishedAt *int64 `json:"finished_at"`
	Status     string `json:"status"`
	Detail     string `json:"detail"`
}
