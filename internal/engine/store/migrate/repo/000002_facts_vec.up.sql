CREATE VIRTUAL TABLE IF NOT EXISTS facts_vec USING vec0(embedding FLOAT[768] distance_metric=cosine);
