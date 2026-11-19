CREATE TABLE report_results (
  job_id text PRIMARY KEY CHECK (length(job_id) BETWEEN 1 AND 80),
  title text NOT NULL CHECK (length(title) BETWEEN 1 AND 100)
);
