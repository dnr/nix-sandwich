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

resource "aws_ecr_repository" "repo" {
  name = "nix-sandwich-differ"
}

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
}

resource "aws_lambda_function_url" "differ" {
  function_name      = aws_lambda_function.differ.function_name
  authorization_type = "NONE"
  invoke_mode        = "RESPONSE_STREAM"
}
