--
-- PostgreSQL database dump
--

\restrict Bod7k5Q970hVlhKY2Sgow1lwHbitJ4gbJ3caBv910o7TCk9poDIS8coaedBt2TB

-- Dumped from database version 17.9
-- Dumped by pg_dump version 17.9

SET statement_timeout = 0;
SET lock_timeout = 0;
SET idle_in_transaction_session_timeout = 0;
SET transaction_timeout = 0;
SET client_encoding = 'UTF8';
SET standard_conforming_strings = on;
SELECT pg_catalog.set_config('search_path', '', false);
SET check_function_bodies = false;
SET xmloption = content;
SET client_min_messages = warning;
SET row_security = off;

SET default_tablespace = '';

SET default_table_access_method = heap;

--
-- Name: api_tokens; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.api_tokens (
    id uuid NOT NULL,
    user_id uuid NOT NULL,
    token_hash text NOT NULL,
    name text DEFAULT ''::text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    last_used_at timestamp with time zone
);


--
-- Name: blob_bot_files; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.blob_bot_files (
    blob_id uuid NOT NULL,
    bot_id uuid NOT NULL,
    file_id text NOT NULL,
    file_unique_id text DEFAULT ''::text NOT NULL,
    fetched_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: blobs; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.blobs (
    id uuid NOT NULL,
    channel_id uuid NOT NULL,
    message_id bigint NOT NULL,
    message_seq bigint DEFAULT 0 NOT NULL,
    size bigint DEFAULT 0 NOT NULL,
    content_hash bytea NOT NULL,
    state integer NOT NULL,
    refcount bigint DEFAULT 0 NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    sealed_at timestamp with time zone
);


--
-- Name: bot_channel; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.bot_channel (
    bot_id uuid NOT NULL,
    channel_id uuid NOT NULL,
    member boolean DEFAULT false NOT NULL,
    checked_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: bots; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.bots (
    id uuid NOT NULL,
    username text DEFAULT ''::text NOT NULL,
    token_sha text NOT NULL,
    token_enc bytea NOT NULL,
    enabled boolean DEFAULT true NOT NULL,
    unavailable_until timestamp with time zone,
    created_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: channels; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.channels (
    id uuid NOT NULL,
    tg_chat_id bigint NOT NULL,
    title text DEFAULT ''::text NOT NULL,
    message_counter bigint DEFAULT 0 NOT NULL,
    eviction_threshold bigint DEFAULT 900000 NOT NULL,
    available boolean DEFAULT true NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: events; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.events (
    id uuid NOT NULL,
    ts timestamp with time zone DEFAULT now() NOT NULL,
    kind text NOT NULL,
    message text DEFAULT ''::text NOT NULL,
    ref text DEFAULT ''::text NOT NULL
);


--
-- Name: extents; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.extents (
    id uuid NOT NULL,
    node_id uuid NOT NULL,
    seq bigint NOT NULL,
    file_offset bigint NOT NULL,
    length bigint NOT NULL,
    blob_id uuid NOT NULL,
    blob_offset bigint NOT NULL
);


--
-- Name: nodes; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.nodes (
    id uuid NOT NULL,
    user_id uuid NOT NULL,
    parent_id uuid,
    name text NOT NULL,
    path text NOT NULL,
    is_dir boolean NOT NULL,
    size bigint DEFAULT 0 NOT NULL,
    content_hash text DEFAULT ''::text NOT NULL,
    etag text DEFAULT ''::text NOT NULL,
    content_type text DEFAULT ''::text NOT NULL,
    state integer DEFAULT 2 NOT NULL,
    packer_lease_owner text DEFAULT ''::text NOT NULL,
    packer_lease_until timestamp with time zone,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    modified_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: schema_migrations; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.schema_migrations (
    version character varying NOT NULL
);


--
-- Name: settings; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.settings (
    id integer DEFAULT 1 NOT NULL,
    blob_max_size bigint NOT NULL,
    wal_idle_timeout_ms bigint NOT NULL,
    max_file_size bigint DEFAULT 0 NOT NULL,
    default_eviction_threshold bigint DEFAULT 900000 NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT settings_id_check CHECK ((id = 1))
);


--
-- Name: stat_samples; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.stat_samples (
    id uuid NOT NULL,
    ts timestamp with time zone DEFAULT now() NOT NULL,
    metric text NOT NULL,
    label text DEFAULT ''::text NOT NULL,
    value double precision NOT NULL
);


--
-- Name: users; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.users (
    id uuid NOT NULL,
    login text NOT NULL,
    password_hash text NOT NULL,
    is_admin boolean DEFAULT false NOT NULL,
    quota_bytes bigint DEFAULT 0 NOT NULL,
    bandwidth_bps bigint DEFAULT 0 NOT NULL,
    rate_per_min integer DEFAULT 0 NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: wal_chunks; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.wal_chunks (
    id uuid NOT NULL,
    node_id uuid NOT NULL,
    seq bigint NOT NULL,
    data bytea NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: api_tokens api_tokens_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.api_tokens
    ADD CONSTRAINT api_tokens_pkey PRIMARY KEY (id);


--
-- Name: api_tokens api_tokens_token_hash_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.api_tokens
    ADD CONSTRAINT api_tokens_token_hash_key UNIQUE (token_hash);


--
-- Name: blob_bot_files blob_bot_files_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.blob_bot_files
    ADD CONSTRAINT blob_bot_files_pkey PRIMARY KEY (blob_id, bot_id);


--
-- Name: blobs blobs_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.blobs
    ADD CONSTRAINT blobs_pkey PRIMARY KEY (id);


--
-- Name: bot_channel bot_channel_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.bot_channel
    ADD CONSTRAINT bot_channel_pkey PRIMARY KEY (bot_id, channel_id);


--
-- Name: bots bots_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.bots
    ADD CONSTRAINT bots_pkey PRIMARY KEY (id);


--
-- Name: bots bots_token_sha_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.bots
    ADD CONSTRAINT bots_token_sha_key UNIQUE (token_sha);


--
-- Name: channels channels_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.channels
    ADD CONSTRAINT channels_pkey PRIMARY KEY (id);


--
-- Name: channels channels_tg_chat_id_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.channels
    ADD CONSTRAINT channels_tg_chat_id_key UNIQUE (tg_chat_id);


--
-- Name: events events_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.events
    ADD CONSTRAINT events_pkey PRIMARY KEY (id);


--
-- Name: extents extents_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.extents
    ADD CONSTRAINT extents_pkey PRIMARY KEY (id);


--
-- Name: nodes nodes_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.nodes
    ADD CONSTRAINT nodes_pkey PRIMARY KEY (id);


--
-- Name: nodes nodes_user_id_path_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.nodes
    ADD CONSTRAINT nodes_user_id_path_key UNIQUE (user_id, path);


--
-- Name: schema_migrations schema_migrations_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.schema_migrations
    ADD CONSTRAINT schema_migrations_pkey PRIMARY KEY (version);


--
-- Name: settings settings_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.settings
    ADD CONSTRAINT settings_pkey PRIMARY KEY (id);


--
-- Name: stat_samples stat_samples_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.stat_samples
    ADD CONSTRAINT stat_samples_pkey PRIMARY KEY (id);


--
-- Name: users users_login_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.users
    ADD CONSTRAINT users_login_key UNIQUE (login);


--
-- Name: users users_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.users
    ADD CONSTRAINT users_pkey PRIMARY KEY (id);


--
-- Name: wal_chunks wal_chunks_node_id_seq_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.wal_chunks
    ADD CONSTRAINT wal_chunks_node_id_seq_key UNIQUE (node_id, seq);


--
-- Name: wal_chunks wal_chunks_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.wal_chunks
    ADD CONSTRAINT wal_chunks_pkey PRIMARY KEY (id);


--
-- Name: idx_api_tokens_user; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_api_tokens_user ON public.api_tokens USING btree (user_id);


--
-- Name: idx_blobs_channel; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_blobs_channel ON public.blobs USING btree (channel_id);


--
-- Name: idx_blobs_channel_seq; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_blobs_channel_seq ON public.blobs USING btree (channel_id, message_seq);


--
-- Name: idx_blobs_collectable; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_blobs_collectable ON public.blobs USING btree (refcount) WHERE (refcount <= 0);


--
-- Name: idx_blobs_state; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_blobs_state ON public.blobs USING btree (state);


--
-- Name: idx_bot_channel_channel; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_bot_channel_channel ON public.bot_channel USING btree (channel_id);


--
-- Name: idx_events_kind; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_events_kind ON public.events USING btree (kind);


--
-- Name: idx_events_ts; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_events_ts ON public.events USING btree (ts DESC);


--
-- Name: idx_extents_blob; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_extents_blob ON public.extents USING btree (blob_id);


--
-- Name: idx_extents_node; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_extents_node ON public.extents USING btree (node_id, seq);


--
-- Name: idx_nodes_pack; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_nodes_pack ON public.nodes USING btree (packer_lease_until) WHERE (state = 1);


--
-- Name: idx_nodes_user; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_nodes_user ON public.nodes USING btree (user_id);


--
-- Name: idx_nodes_user_parent; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_nodes_user_parent ON public.nodes USING btree (user_id, parent_id);


--
-- Name: idx_stat_samples_metric; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_stat_samples_metric ON public.stat_samples USING btree (metric, label, ts DESC);


--
-- Name: idx_wal_node_seq; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_wal_node_seq ON public.wal_chunks USING btree (node_id, seq);


--
-- Name: api_tokens api_tokens_user_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.api_tokens
    ADD CONSTRAINT api_tokens_user_id_fkey FOREIGN KEY (user_id) REFERENCES public.users(id) ON DELETE CASCADE;


--
-- Name: blob_bot_files blob_bot_files_blob_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.blob_bot_files
    ADD CONSTRAINT blob_bot_files_blob_id_fkey FOREIGN KEY (blob_id) REFERENCES public.blobs(id) ON DELETE CASCADE;


--
-- Name: blob_bot_files blob_bot_files_bot_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.blob_bot_files
    ADD CONSTRAINT blob_bot_files_bot_id_fkey FOREIGN KEY (bot_id) REFERENCES public.bots(id) ON DELETE CASCADE;


--
-- Name: blobs blobs_channel_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.blobs
    ADD CONSTRAINT blobs_channel_id_fkey FOREIGN KEY (channel_id) REFERENCES public.channels(id) ON DELETE RESTRICT;


--
-- Name: bot_channel bot_channel_bot_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.bot_channel
    ADD CONSTRAINT bot_channel_bot_id_fkey FOREIGN KEY (bot_id) REFERENCES public.bots(id) ON DELETE CASCADE;


--
-- Name: bot_channel bot_channel_channel_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.bot_channel
    ADD CONSTRAINT bot_channel_channel_id_fkey FOREIGN KEY (channel_id) REFERENCES public.channels(id) ON DELETE CASCADE;


--
-- Name: extents extents_blob_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.extents
    ADD CONSTRAINT extents_blob_id_fkey FOREIGN KEY (blob_id) REFERENCES public.blobs(id) ON DELETE RESTRICT;


--
-- Name: extents extents_node_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.extents
    ADD CONSTRAINT extents_node_id_fkey FOREIGN KEY (node_id) REFERENCES public.nodes(id) ON DELETE CASCADE;


--
-- Name: nodes nodes_parent_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.nodes
    ADD CONSTRAINT nodes_parent_id_fkey FOREIGN KEY (parent_id) REFERENCES public.nodes(id) ON DELETE CASCADE;


--
-- Name: nodes nodes_user_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.nodes
    ADD CONSTRAINT nodes_user_id_fkey FOREIGN KEY (user_id) REFERENCES public.users(id) ON DELETE CASCADE;


--
-- Name: wal_chunks wal_chunks_node_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.wal_chunks
    ADD CONSTRAINT wal_chunks_node_id_fkey FOREIGN KEY (node_id) REFERENCES public.nodes(id) ON DELETE CASCADE;


--
-- PostgreSQL database dump complete
--

\unrestrict Bod7k5Q970hVlhKY2Sgow1lwHbitJ4gbJ3caBv910o7TCk9poDIS8coaedBt2TB

