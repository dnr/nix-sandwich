
// iam for lambda:

data "aws_iam_policy_document" "assume_role" {
  statement {
    principals {
      type        = "Service"
      identifiers = ["lambda.amazonaws.com"]
    }
    actions = ["sts:AssumeRole"]
  }
}

resource "aws_iam_role" "iam_for_lambda" {
  name                = "iam_for_lambda"
  assume_role_policy  = data.aws_iam_policy_document.assume_role.json
  managed_policy_arns = ["arn:aws:iam::aws:policy/service-role/AWSLambdaBasicExecutionRole"]
}

// ecr:

resource "aws_ecr_repository" "repo" {
  name = "nix-sandwich-differ"
}

// s3:

resource "aws_s3_bucket" "cache" {
  bucket = "nxsdch-cache-1"
}

data "aws_iam_policy_document" "cache_bucket_policy" {
  statement {
    principals {
      type        = "*"
      identifiers = ["*"]
    }
    actions   = ["s3:GetObject"]
    resources = [aws_s3_bucket.cache.arn, "${aws_s3_bucket.cache.arn}/*"]
  }
  statement {
    principals {
      type        = "AWS"
      identifiers = [aws_iam_role.iam_for_lambda.arn]
    }
    actions   = ["s3:*"]
    resources = [aws_s3_bucket.cache.arn, "${aws_s3_bucket.cache.arn}/*"]
  }
}

resource "aws_s3_bucket_policy" "cache" {
  bucket = aws_s3_bucket.cache.id
  policy = data.aws_iam_policy_document.cache_bucket_policy.json
}

resource "aws_s3_bucket_lifecycle_configuration" "cache" {
  bucket = aws_s3_bucket.cache.id
  rule {
    id     = "ttl"
    status = "Enabled"
    abort_incomplete_multipart_upload { days_after_initiation = 1 }
    expiration { days = 7 }
  }
}

resource "aws_s3_bucket_public_access_block" "cache" {
  bucket                  = aws_s3_bucket.cache.id
  block_public_acls       = false
  block_public_policy     = false
  ignore_public_acls      = false
  restrict_public_buckets = false
}

// lambda:

variable "differ_image_tag" {}

resource "aws_lambda_function" "differ" {
  package_type = "Image"
  image_uri    = "${aws_ecr_repository.repo.repository_url}:${var.differ_image_tag}"

  function_name = "nix-sandwich-differ"
  role          = aws_iam_role.iam_for_lambda.arn

  architectures = ["x86_64"] # TODO: can we make it run on arm?

  memory_size = 1800 # MB
  ephemeral_storage {
    size = 2048 # MB
  }
  timeout = 300 # seconds
  environment {
    variables = {
      // must be in the same region:
      nix_sandwich_cache_write_s3_bucket = aws_s3_bucket.cache.id
    }
  }
}

resource "aws_lambda_function_url" "differ" {
  function_name      = aws_lambda_function.differ.function_name
  authorization_type = "NONE"
  invoke_mode        = "RESPONSE_STREAM"
}
