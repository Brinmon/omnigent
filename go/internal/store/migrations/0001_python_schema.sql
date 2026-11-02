-- account_tokens
CREATE TABLE "account_tokens" (
	id VARCHAR(128) NOT NULL, 
	user_id VARCHAR(128), 
	created_by VARCHAR(128), 
	created_at INTEGER NOT NULL, 
	expires_at INTEGER NOT NULL, 
	redeemed_at INTEGER, 
	invited_is_admin BOOLEAN DEFAULT 0 NOT NULL, 
	workspace_id BIGINT DEFAULT '0' NOT NULL, 
	kind SMALLINT NOT NULL, account_generation VARCHAR(32), 
	CONSTRAINT pk_account_tokens PRIMARY KEY (workspace_id, id), 
	CONSTRAINT ck_account_tokens_kind CHECK (kind IN (1, 2))
);

-- agents
CREATE TABLE "agents" (
	id BLOB NOT NULL, 
	created_at INTEGER NOT NULL, 
	name VARCHAR(256) NOT NULL, 
	bundle_location VARCHAR(512) NOT NULL, 
	version INTEGER DEFAULT '1' NOT NULL, 
	description BLOB, 
	updated_at INTEGER, 
	workspace_id BIGINT DEFAULT '0' NOT NULL, 
	kind SMALLINT NOT NULL, created_by VARCHAR(128), 
	CONSTRAINT pk_agents PRIMARY KEY (workspace_id, id), 
	CONSTRAINT ck_agents_kind CHECK (kind IN (1, 2))
);

-- comments
CREATE TABLE "comments" (
	id BLOB NOT NULL, 
	conversation_id BLOB NOT NULL, 
	path VARCHAR(4096) NOT NULL, 
	body BLOB NOT NULL, 
	created_at INTEGER NOT NULL, 
	anchor_content BLOB, 
	start_index INTEGER DEFAULT '0' NOT NULL, 
	end_index INTEGER DEFAULT '0' NOT NULL, 
	created_by VARCHAR(128), 
	updated_at BIGINT NOT NULL, 
	workspace_id BIGINT DEFAULT '0' NOT NULL, 
	status SMALLINT NOT NULL, 
	CONSTRAINT pk_comments PRIMARY KEY (workspace_id, conversation_id, id), 
	CONSTRAINT ck_comments_status CHECK (status IN (1, 2))
);

-- connections
CREATE TABLE connections (
	workspace_id BIGINT DEFAULT '0' NOT NULL, 
	user_id VARCHAR(128) NOT NULL, 
	provider VARCHAR(64) NOT NULL, 
	account_id VARCHAR(128) DEFAULT '' NOT NULL, 
	secret_enc TEXT NOT NULL, 
	metadata_json TEXT NOT NULL, 
	created_at INTEGER NOT NULL, 
	updated_at INTEGER NOT NULL, 
	PRIMARY KEY (workspace_id, user_id, provider, account_id)
);

-- conversation_items
CREATE TABLE "conversation_items" (
	id BLOB NOT NULL, 
	conversation_id BLOB NOT NULL, 
	response_id VARCHAR(64) NOT NULL, 
	created_at INTEGER NOT NULL, 
	position INTEGER NOT NULL, 
	data TEXT NOT NULL, 
	search_text TEXT NOT NULL, 
	created_by VARCHAR(128), 
	workspace_id BIGINT DEFAULT '0' NOT NULL, 
	type SMALLINT NOT NULL, 
	status SMALLINT NOT NULL, 
	CONSTRAINT pk_conversation_items PRIMARY KEY (workspace_id, conversation_id, id, created_at), 
	CONSTRAINT ck_conversation_items_status CHECK (status IN (1, 2, 3, 4)), 
	CONSTRAINT ck_conversation_items_type CHECK (type IN (1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11))
);

-- conversation_labels
CREATE TABLE "conversation_labels" (
	conversation_id BLOB NOT NULL, 
	"key" VARCHAR(128) NOT NULL, 
	value VARCHAR(256) NOT NULL, 
	updated_at INTEGER NOT NULL, 
	workspace_id BIGINT DEFAULT '0' NOT NULL, 
	CONSTRAINT pk_conversation_labels PRIMARY KEY (workspace_id, conversation_id, "key")
);

-- conversations
CREATE TABLE "conversations" (
	id BLOB NOT NULL, 
	created_at INTEGER NOT NULL, 
	updated_at INTEGER NOT NULL, 
	title VARCHAR(768) DEFAULT ('') NOT NULL, 
	parent_conversation_id BLOB, 
	root_conversation_id BLOB NOT NULL, 
	next_position INTEGER, 
	workspace_id BIGINT DEFAULT '0' NOT NULL, 
	archived BOOLEAN DEFAULT 0 NOT NULL, 
	agent_id BLOB, 
	session_overrides VARCHAR(512), 
	CONSTRAINT pk_conversations PRIMARY KEY (workspace_id, id)
);

-- device_grants
CREATE TABLE device_grants (
	workspace_id BIGINT DEFAULT '0' NOT NULL, 
	id VARCHAR(128) NOT NULL, 
	device_code_hash VARCHAR(64) NOT NULL, 
	user_code VARCHAR(32) NOT NULL, 
	status SMALLINT NOT NULL, 
	client_id VARCHAR(128), 
	user_id VARCHAR(128), 
	refresh_token_hash VARCHAR(64), 
	prev_refresh_token_hash VARCHAR(64), 
	created_at INTEGER NOT NULL, 
	expires_at INTEGER NOT NULL, 
	approved_at INTEGER, 
	last_polled_at INTEGER, account_generation VARCHAR(32), 
	PRIMARY KEY (workspace_id, id), 
	CONSTRAINT ck_device_grants_status CHECK (status IN (1, 2, 3, 4, 5))
);

-- files
CREATE TABLE "files" (
	id BLOB NOT NULL, 
	created_at INTEGER NOT NULL, 
	filename VARCHAR(512) NOT NULL, 
	bytes INTEGER NOT NULL, 
	content_type VARCHAR(256), 
	session_id BLOB, 
	workspace_id BIGINT DEFAULT '0' NOT NULL, source_metadata TEXT, blob_key BLOB, 
	CONSTRAINT pk_files PRIMARY KEY (workspace_id, id)
);

-- hosts
CREATE TABLE "hosts" (
	workspace_id BIGINT DEFAULT '0' NOT NULL, 
	host_id BLOB NOT NULL, 
	user_id VARCHAR(128) NOT NULL, 
	name VARCHAR(64) NOT NULL, 
	status SMALLINT NOT NULL, 
	created_at INTEGER, 
	updated_at INTEGER, 
	token_hash VARCHAR(64), 
	token_expires_at INTEGER, 
	sandbox_provider VARCHAR(32), 
	sandbox_id VARCHAR(256), 
	configured_harnesses BLOB, 
	terminating_sandbox_id VARCHAR(256), 
	deleted_at BIGINT, account_generation VARCHAR(32), 
	CONSTRAINT pk_hosts PRIMARY KEY (workspace_id, host_id), 
	CONSTRAINT uq_hosts_workspace_user_id_name UNIQUE (workspace_id, user_id, name), 
	CONSTRAINT ck_hosts_status CHECK (status IN (1, 2))
);

-- omnigent_conversation_metadata
CREATE TABLE "omnigent_conversation_metadata" (
	workspace_id BIGINT DEFAULT '0' NOT NULL, 
	id BLOB NOT NULL, 
	kind SMALLINT DEFAULT '1' NOT NULL, 
	runner_id VARCHAR(64), 
	host_id BLOB, 
	sub_agent_name VARCHAR(128), 
	external_session_id VARCHAR(128), 
	session_state BLOB, 
	session_usage BLOB, 
	terminal_launch_args BLOB, 
	workspace VARCHAR(2048), 
	git_branch VARCHAR(255), 
	runner_last_seen INTEGER, 
	live_status SMALLINT, 
	pending_elicitation_count INTEGER, 
	project_id BLOB, 
	task_summary VARCHAR(128), 
	inference_snapshot BLOB, pull_requests BLOB, 
	PRIMARY KEY (workspace_id, id), 
	CONSTRAINT ck_conversation_metadata_kind CHECK (kind IN (1, 2)), 
	CONSTRAINT ck_conversation_metadata_workspace_required_for_host CHECK (host_id IS NULL OR workspace IS NOT NULL)
);

-- policies
CREATE TABLE "policies" (
	id BLOB NOT NULL, 
	name VARCHAR(256) NOT NULL, 
	created_at INTEGER NOT NULL, 
	updated_at INTEGER, 
	enabled BOOLEAN DEFAULT 1 NOT NULL, 
	session_id BLOB, 
	handler BLOB, 
	factory_params BLOB, 
	created_by VARCHAR(128), 
	workspace_id BIGINT DEFAULT '0' NOT NULL, 
	type SMALLINT NOT NULL, 
	scope SMALLINT NOT NULL, 
	name_cksum BLOB NOT NULL, 
	CONSTRAINT pk_policies PRIMARY KEY (workspace_id, id), 
	CONSTRAINT ck_policies_scope CHECK (scope IN (1, 2)), 
	CONSTRAINT ck_policies_type CHECK (type IN (1, 2))
);

-- preferences
CREATE TABLE preferences (
	workspace_id BIGINT DEFAULT '0' NOT NULL, 
	user_id VARCHAR(128) NOT NULL, 
	"key" VARCHAR(128) NOT NULL, 
	value BLOB NOT NULL, 
	PRIMARY KEY (workspace_id, user_id, "key")
);

-- projects
CREATE TABLE "projects" (
	workspace_id BIGINT DEFAULT '0' NOT NULL, 
	id BLOB NOT NULL, 
	name VARCHAR(256) NOT NULL, 
	user_id VARCHAR(128), 
	created_at INTEGER NOT NULL, 
	updated_at INTEGER, 
	config BLOB, 
	PRIMARY KEY (workspace_id, id)
);

-- scheduled_task_runs
CREATE TABLE "scheduled_task_runs" (
	workspace_id BIGINT DEFAULT '0' NOT NULL, 
	id BLOB NOT NULL, 
	scheduled_task_id BLOB NOT NULL, 
	conversation_id BLOB, 
	status SMALLINT NOT NULL, 
	scheduled_at BIGINT NOT NULL, 
	fired_at BIGINT, 
	finished_at BIGINT, 
	error BLOB, 
	error_code VARCHAR(64), 
	trigger_key VARCHAR(256), 
	host_id BLOB, 
	lease_expires_at BIGINT, 
	PRIMARY KEY (workspace_id, id), 
	CONSTRAINT ck_scheduled_task_runs_dispatch_status CHECK (status IN (1, 2, 3, 4, 5, 6)), 
	CONSTRAINT uq_scheduled_task_runs_occurrence UNIQUE (workspace_id, scheduled_task_id, trigger_key)
);

-- scheduled_tasks
CREATE TABLE "scheduled_tasks" (
	workspace_id BIGINT DEFAULT '0' NOT NULL, 
	id BLOB NOT NULL, 
	name VARCHAR(256) NOT NULL, 
	prompt BLOB NOT NULL, 
	user_id VARCHAR(128), 
	agent_id BLOB NOT NULL, 
	model_override VARCHAR(128), 
	reasoning_effort VARCHAR(32), 
	workspace VARCHAR(2048), 
	base_branch VARCHAR(255), 
	execution_target SMALLINT DEFAULT '1' NOT NULL, 
	host_id BLOB, 
	timezone VARCHAR(64) DEFAULT 'UTC' NOT NULL, 
	state SMALLINT DEFAULT '1' NOT NULL, 
	last_run_at BIGINT, 
	last_run_conversation_id BLOB, 
	created_at BIGINT NOT NULL, 
	updated_at BIGINT, 
	rrule VARCHAR(512) NOT NULL, 
	max_cost_usd FLOAT, 
	permission_mode VARCHAR(32), 
	account_generation VARCHAR(32), 
	next_run_at BIGINT, 
	project_id BLOB, 
	delegation_ref VARCHAR(1024), 
	session_mode SMALLINT DEFAULT '1' NOT NULL, 
	session_id BLOB, 
	PRIMARY KEY (workspace_id, id), 
	CONSTRAINT ck_scheduled_tasks_lifecycle_state CHECK (state IN (1, 2, 3, 4)), 
	CONSTRAINT ck_scheduled_tasks_execution_target CHECK (execution_target IN (1, 2)), 
	CONSTRAINT ck_scheduled_tasks_session_mode CHECK (session_mode IN (1, 2))
);

-- session_permissions
CREATE TABLE "session_permissions" (
	user_id VARCHAR(128) NOT NULL, 
	conversation_id BLOB NOT NULL, 
	level INTEGER NOT NULL, 
	workspace_id BIGINT DEFAULT '0' NOT NULL, 
	CONSTRAINT pk_session_permissions PRIMARY KEY (workspace_id, user_id, conversation_id), 
	CONSTRAINT ck_session_permissions_level CHECK (level IN (1, 2, 3, 4))
);

-- user_daily_cost
CREATE TABLE "user_daily_cost" (
	user_id VARCHAR(128) NOT NULL, 
	day_utc VARCHAR(10) NOT NULL, 
	cost_usd FLOAT DEFAULT '0' NOT NULL, 
	updated_at INTEGER NOT NULL, 
	ask_approved_usd FLOAT DEFAULT '0' NOT NULL, 
	workspace_id BIGINT DEFAULT '0' NOT NULL, 
	CONSTRAINT pk_user_daily_cost PRIMARY KEY (workspace_id, user_id, day_utc)
);

-- users
CREATE TABLE "users" (
	id VARCHAR(128) NOT NULL, 
	is_admin BOOLEAN DEFAULT 0 NOT NULL, 
	password_hash VARCHAR(256), 
	created_at INTEGER, 
	last_login_at INTEGER, 
	workspace_id BIGINT DEFAULT '0' NOT NULL, 
	account_generation VARCHAR(32), 
	deleted_at INTEGER, 
	CONSTRAINT pk_users PRIMARY KEY (workspace_id, id)
);
