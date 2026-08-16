-- ============================================================================
-- ApplyMind — demo data reset (v3: user_id-scoped, 100 applications, every
-- real site used)
--
-- Clears every application, CV, and cover letter belonging to the demo
-- account, then generates 100 realistic applications rotating through all 11
-- pre-configured sites. settings.profile_summary is left untouched — that's
-- your real elevator pitch, not fake data.
--
-- This is generative (a DO block + generate_series), not 100 hand-typed rows —
-- at this size that's the only sane way to keep it both correct and reviewable.
-- Re-running it gives identical output every time; nothing here is random.
--
-- Phase 15: every row this script writes now carries user_id, resolved once at
-- the top from the demo account's email (migration 000016 seeds that account).
-- If that account does not exist yet, this script fails fast on the SELECT
-- INTO STRICT below rather than silently inserting rowgis with a NULL user_id
-- that the NOT NULL constraint would reject anyway, but with a much less
-- useful error.
--
-- Run via the Neon SQL editor (paste, run), or:
--   psql "$NEON_DATABASE_URL" -f seed-demo-data.sql
-- ============================================================================

BEGIN;

-- ----------------------------------------------------------------------------
-- 0. Resolve the demo user. Every insert below is scoped to this id.
-- ----------------------------------------------------------------------------
DO $$
DECLARE
    demo_user_id uuid;
BEGIN
    SELECT id INTO STRICT demo_user_id
    FROM users
    WHERE email = 'demo@applymind.faroukhasnaoui.tech';

    -- Stashed in a session-local temp table so every later statement in this
    -- script — not just this DO block — can read it without repeating the
    -- lookup or re-declaring the variable in a scope that cannot see it.
    CREATE TEMPORARY TABLE IF NOT EXISTS _seed_context (user_id uuid) ON COMMIT DROP;
    DELETE FROM _seed_context;
    INSERT INTO _seed_context (user_id) VALUES (demo_user_id);
END $$;

-- ----------------------------------------------------------------------------
-- 1. Clear this user's existing application data, children first. Scoped by
--    user_id throughout — a fresh demo reset must not touch any other
--    account's rows, even though in practice this script only ever runs
--    against a database where the demo account is the only real user.
-- ----------------------------------------------------------------------------
DELETE FROM follow_up_reminders WHERE user_id = (SELECT user_id FROM _seed_context);
DELETE FROM recruiter_contacts WHERE user_id = (SELECT user_id FROM _seed_context);
DELETE FROM application_status_history WHERE user_id = (SELECT user_id FROM _seed_context);
DELETE FROM cover_letters WHERE user_id = (SELECT user_id FROM _seed_context);
DELETE FROM applications WHERE user_id = (SELECT user_id FROM _seed_context);
DELETE FROM cv_versions WHERE user_id = (SELECT user_id FROM _seed_context);
DELETE FROM cvs WHERE user_id = (SELECT user_id FROM _seed_context);

-- Remove this user's custom sites from previous runs — this script uses only
-- the pre-configured (global) sites already in the database, all 11 of them.
DELETE FROM sites WHERE is_preconfigured = false AND user_id = (SELECT user_id FROM _seed_context);

-- ----------------------------------------------------------------------------
-- 2. CVs and CV versions — four CVs, ten versions total, so "which CV went
--    where" has real variety across 100 applications.
-- ----------------------------------------------------------------------------
INSERT INTO cvs (id, user_id, name, tag, created_at, updated_at)
SELECT * FROM (VALUES
  ('a1000000-0000-4000-8000-000000000001'::uuid, (SELECT user_id FROM _seed_context), 'Backend CV',    'backend',   now() - interval '120 days', now() - interval '10 days'),
  ('a1000000-0000-4000-8000-000000000002'::uuid, (SELECT user_id FROM _seed_context), 'Full Stack CV', 'fullstack', now() - interval '100 days', now() - interval '20 days'),
  ('a1000000-0000-4000-8000-000000000003'::uuid, (SELECT user_id FROM _seed_context), 'Platform CV',   'platform',  now() - interval '80 days',  now() - interval '15 days'),
  ('a1000000-0000-4000-8000-000000000004'::uuid, (SELECT user_id FROM _seed_context), 'Staff CV',      'staff',     now() - interval '30 days',  now() - interval '5 days')
) AS v (id, user_id, name, tag, created_at, updated_at);

INSERT INTO cv_versions (id, user_id, cv_id, sha256_hash, file_size_bytes, original_filename, s3_key, uploaded_at)
SELECT * FROM (VALUES
  ('b1000000-0000-4000-8000-000000000001'::uuid, (SELECT user_id FROM _seed_context), 'a1000000-0000-4000-8000-000000000001'::uuid, repeat('a1', 32), 241664, 'Backend_CV_v1.pdf',   'cvs/demo/backend-cv-v1.pdf',   now() - interval '120 days'),
  ('b1000000-0000-4000-8000-000000000002'::uuid, (SELECT user_id FROM _seed_context), 'a1000000-0000-4000-8000-000000000001'::uuid, repeat('a2', 32), 244992, 'Backend_CV_v2.pdf',   'cvs/demo/backend-cv-v2.pdf',   now() - interval '80 days'),
  ('b1000000-0000-4000-8000-000000000003'::uuid, (SELECT user_id FROM _seed_context), 'a1000000-0000-4000-8000-000000000001'::uuid, repeat('a3', 32), 247808, 'Backend_CV_v3.pdf',   'cvs/demo/backend-cv-v3.pdf',   now() - interval '40 days'),
  ('b1000000-0000-4000-8000-000000000004'::uuid, (SELECT user_id FROM _seed_context), 'a1000000-0000-4000-8000-000000000001'::uuid, repeat('a4', 32), 249856, 'Backend_CV_v4.pdf',   'cvs/demo/backend-cv-v4.pdf',   now() - interval '10 days'),
  ('b2000000-0000-4000-8000-000000000001'::uuid, (SELECT user_id FROM _seed_context), 'a1000000-0000-4000-8000-000000000002'::uuid, repeat('b1', 32), 255488, 'FullStack_CV_v1.pdf', 'cvs/demo/fullstack-cv-v1.pdf', now() - interval '100 days'),
  ('b2000000-0000-4000-8000-000000000002'::uuid, (SELECT user_id FROM _seed_context), 'a1000000-0000-4000-8000-000000000002'::uuid, repeat('b2', 32), 258048, 'FullStack_CV_v2.pdf', 'cvs/demo/fullstack-cv-v2.pdf', now() - interval '20 days'),
  ('b3000000-0000-4000-8000-000000000001'::uuid, (SELECT user_id FROM _seed_context), 'a1000000-0000-4000-8000-000000000003'::uuid, repeat('c1', 32), 252416, 'Platform_CV_v1.pdf',  'cvs/demo/platform-cv-v1.pdf',  now() - interval '80 days'),
  ('b3000000-0000-4000-8000-000000000002'::uuid, (SELECT user_id FROM _seed_context), 'a1000000-0000-4000-8000-000000000003'::uuid, repeat('c2', 32), 253952, 'Platform_CV_v2.pdf',  'cvs/demo/platform-cv-v2.pdf',  now() - interval '15 days'),
  ('b4000000-0000-4000-8000-000000000001'::uuid, (SELECT user_id FROM _seed_context), 'a1000000-0000-4000-8000-000000000004'::uuid, repeat('d1', 32), 260096, 'Staff_CV_v1.pdf',     'cvs/demo/staff-cv-v1.pdf',     now() - interval '30 days'),
  ('b4000000-0000-4000-8000-000000000002'::uuid, (SELECT user_id FROM _seed_context), 'a1000000-0000-4000-8000-000000000004'::uuid, repeat('d2', 32), 261632, 'Staff_CV_v2.pdf',     'cvs/demo/staff-cv-v2.pdf',     now() - interval '5 days')
) AS v (id, user_id, cv_id, sha256_hash, file_size_bytes, original_filename, s3_key, uploaded_at);

-- ----------------------------------------------------------------------------
-- 3. Generate 100 applications, cycling through every site, ~50 companies,
--    12 role titles, and all 11 statuses evenly.
-- ----------------------------------------------------------------------------
DO $$
DECLARE
  demo_user_id uuid := (SELECT user_id FROM _seed_context);

  companies text[] := ARRAY[
    'Stripe','Datadog','Monzo','Cloudflare','Vercel','Notion','Linear','Figma','GitHub','Shopify',
    'Airbnb','Revolut','Spotify','Duolingo','Netflix','Discord','Slack','Atlassian','GitLab','HashiCorp',
    'Snowflake','MongoDB','Elastic','Confluent','PagerDuty','Twilio','Segment','Amplitude','Plaid','Brex',
    'Ramp','Deel','Remote','Canva','Miro','Asana','Airtable','Retool','Supabase','PlanetScale',
    'Neon','Render','Railway','Fly.io','Clerk','Auth0','Okta','CrowdStrike','Snyk','Sentry','LaunchDarkly'
  ];
  titles text[] := ARRAY[
    'Backend Engineer','Platform Engineer','Software Engineer','Full Stack Engineer',
    'Infrastructure Engineer','Site Reliability Engineer','Systems Engineer','Go Engineer',
    'Senior Backend Engineer','Staff Engineer','Founding Engineer','DevOps Engineer'
  ];
  site_domains text[] := ARRAY[
    'linkedin.com','indeed.com','glassdoor.com','greenhouse.io','lever.co','wellfound.com',
    'hired.com','monster.com','myworkdayjobs.com','irishjobs.ie','jobs.ie'
  ];
  statuses text[] := ARRAY[
    'Saved','Applied','Acknowledged','In Review','Interview Scheduled',
    'Interviewing','Offer Received','Accepted','Rejected','Withdrawn','Ghost'
  ];
  description_templates text[] := ARRAY[
    '%s is hiring a %s to help build and scale core product infrastructure. Strong communication and collaboration skills expected.',
    'Join %s as a %s working across backend services that support a large and growing user base.',
    '%s is looking for an experienced %s to take ownership of key systems and mentor engineers on the team.',
    '%s''s engineering team is expanding and looking for a %s to help ship reliable, well-tested software at scale.'
  ];
  explanation_templates text[] := ARRAY[
    'Strong overall match against the stored profile summary.',
    'Good technical overlap; some domain-specific experience is limited.',
    'Solid general backend fit with the responsibilities described.',
    'Close match on the core technical requirements listed.',
    'Reasonable fit; the role leans slightly outside the primary area of experience.'
  ];
  terminal_notes text[] := ARRAY[
    'Went with a candidate with more directly relevant experience.',
    'Position was put on hold by the team.',
    'Accepted an offer elsewhere before this process finished.',
    'No response after the most recent interview.',
    'Verbal offer received, paperwork in progress.',
    'Signed. Starting next month.',
    'Great first conversation, moving to the next round.'
  ];

  version_ids uuid[];
  version_count int;

  company text;
  title text;
  site_domain text;
  site_id_var uuid;
  status_var text;
  path text[];
  path_len int;

  created_at_var timestamptz;
  applied_at_var timestamptz;
  updated_at_var timestamptz;
  step_interval interval;

  score numeric;
  explanation text;
  cv_version_id_var uuid;

  app_id uuid;
  job_url_var text;
  description_var text;

  j int;
  changed_at_var timestamptz;
  changed_by_var text;
  note_var text;
BEGIN
  SELECT array_agg(id ORDER BY uploaded_at), count(*)
    INTO version_ids, version_count
    FROM cv_versions
    WHERE user_id = demo_user_id;

  FOR i IN 1..100 LOOP
    company     := companies[1 + ((i - 1) % array_length(companies, 1))];
    title       := titles[1 + ((i - 1) % array_length(titles, 1))];
    site_domain := site_domains[1 + ((i - 1) % array_length(site_domains, 1))];
    status_var  := statuses[1 + ((i - 1) % array_length(statuses, 1))];

    -- Pre-configured sites are global (user_id IS NULL); this script never
    -- creates custom ones, so there is nothing to scope this lookup by.
    SELECT id INTO site_id_var FROM sites WHERE domain = site_domain AND user_id IS NULL;

    path := CASE status_var
      WHEN 'Saved'                THEN ARRAY['Saved']
      WHEN 'Applied'               THEN ARRAY['Applied']
      WHEN 'Acknowledged'          THEN ARRAY['Applied','Acknowledged']
      WHEN 'In Review'             THEN ARRAY['Applied','Acknowledged','In Review']
      WHEN 'Interview Scheduled'   THEN ARRAY['Applied','Acknowledged','In Review','Interview Scheduled']
      WHEN 'Interviewing'          THEN ARRAY['Applied','Acknowledged','In Review','Interview Scheduled','Interviewing']
      WHEN 'Offer Received'        THEN ARRAY['Applied','Acknowledged','In Review','Interview Scheduled','Interviewing','Offer Received']
      WHEN 'Accepted'              THEN ARRAY['Applied','Acknowledged','In Review','Interview Scheduled','Interviewing','Offer Received','Accepted']
      WHEN 'Rejected'              THEN ARRAY['Applied','In Review','Rejected']
      WHEN 'Withdrawn'             THEN ARRAY['Applied','In Review','Interview Scheduled','Withdrawn']
      WHEN 'Ghost'                 THEN ARRAY['Applied','Interview Scheduled','Interviewing','Ghost']
    END;
    path_len := array_length(path, 1);

    IF status_var = 'Saved' THEN
      applied_at_var := NULL;
      created_at_var := now() - (interval '1 day' * ((i % 5) + 1));
      updated_at_var := created_at_var;
      cv_version_id_var := NULL;
      score := NULL;
      explanation := NULL;
    ELSE
      applied_at_var := now() - (interval '1 day' * (((i * 3) % 90) + 5));
      created_at_var := applied_at_var;
      updated_at_var := applied_at_var + (interval '1 day' * (path_len * 3));
      cv_version_id_var := version_ids[1 + ((i - 1) % version_count)];
      score := round((6.0 + (((i * 7) % 40) / 10.0))::numeric, 1);
      explanation := explanation_templates[1 + ((i - 1) % array_length(explanation_templates, 1))];
    END IF;

    app_id := gen_random_uuid();
    job_url_var := format('https://%s/jobs/%s', site_domain, 480000 + i);
    description_var := format(
      description_templates[1 + ((i - 1) % array_length(description_templates, 1))],
      company, title
    );

    INSERT INTO applications (
      id, user_id, company_name, job_title, job_description, job_url, site_id, cv_version_id,
      status, ai_score, ai_score_explanation, applied_at, created_at, updated_at
    ) VALUES (
      app_id, demo_user_id, company, title, description_var, job_url_var, site_id_var, cv_version_id_var,
      status_var::application_status, score, explanation, applied_at_var, created_at_var, updated_at_var
    );

    step_interval := CASE WHEN path_len > 1 THEN (updated_at_var - created_at_var) / (path_len - 1) ELSE interval '0' END;

    FOR j IN 1..path_len LOOP
      changed_at_var := created_at_var + (step_interval * (j - 1));
      changed_by_var := CASE WHEN j = path_len AND path_len > 1 THEN 'user' ELSE 'system' END;
      note_var := CASE
        WHEN j = path_len AND path_len > 1
             AND status_var IN ('Interview Scheduled','Interviewing','Offer Received','Accepted','Rejected','Withdrawn','Ghost')
        THEN terminal_notes[1 + ((i - 1) % array_length(terminal_notes, 1))]
        ELSE NULL
      END;

      INSERT INTO application_status_history (application_id, user_id, from_status, to_status, changed_by, note, changed_at)
      VALUES (
        app_id,
        demo_user_id,
        CASE WHEN j = 1 THEN NULL ELSE path[j - 1]::application_status END,
        path[j]::application_status,
        changed_by_var::status_change_source,
        note_var,
        changed_at_var
      );
    END LOOP;

    IF i % 3 = 0 AND status_var != 'Saved' THEN
      INSERT INTO cover_letters (id, application_id, user_id, kind, body_text, created_at, updated_at)
      VALUES (
        gen_random_uuid(), app_id, demo_user_id, 'text',
        format('I''m excited about the %s role at %s and would welcome the chance to talk through my background in more detail.', title, company),
        created_at_var, created_at_var
      );
    END IF;

    IF i % 4 = 0 AND status_var IN ('Applied','Acknowledged','In Review','Interview Scheduled','Interviewing') THEN
      INSERT INTO follow_up_reminders (application_id, user_id, due_at, sent_at, dismissed_at, created_at, updated_at)
      VALUES (
        app_id, demo_user_id, now() + (interval '1 day' * ((i % 12) + 1)), NULL, NULL, created_at_var, created_at_var
      );
    END IF;

    IF i % 10 = 0 AND status_var IN ('Rejected','Withdrawn') THEN
      INSERT INTO follow_up_reminders (application_id, user_id, due_at, sent_at, dismissed_at, created_at, updated_at)
      VALUES (
        app_id, demo_user_id, updated_at_var, updated_at_var, NULL, created_at_var, updated_at_var
      );
    END IF;

  END LOOP;
END $$;

COMMIT;

-- ============================================================================
-- Verify
-- ============================================================================
-- SELECT status, count(*) FROM applications GROUP BY status ORDER BY status;
-- SELECT s.name, count(*) FROM applications a JOIN sites s ON s.id = a.site_id GROUP BY s.name ORDER BY count(*) DESC;
-- SELECT count(*) FROM cover_letters;
-- SELECT count(*) FROM follow_up_reminders;
