
# nix-sandwich

## What is this and why?

nix-sandwich implements differential compression for downloading from a Nix binary cache,
by acting as a local binary substituer and asking a remote side to compute a binary delta.

The idea is simple:
you want to download `4jszd6d9...-pipewire-0.3.71` as part of a system upgrade.
You already have `xl4851js...-pipewire-0.3.71`.
Instead of just fetching it from the binary cache,
which would take 315060 bytes (with xz compression),
compute a binary delta from the one you have to the one you want,
then apply that to what you have.
It turns out that delta is only 670 bytes, 470× smaller.

This is nearly a best-case because the code didn't actually change,
only its dependencies did, so you might wonder if this is generally applicable.
Certainly, you can't save 400× on all downloads, but it turns out:

1. Because of how Nix works, dependencies-only changes are very common.
2. Binary deltas work quite well even across small (and sometimes large) code
   changes. For example, the `.nar.xz` for `systemd-251.16` is 5816784 bytes,
   but the delta from `systemd-251.15` is only 1270423 bytes, 4.6× smaller.
   Of course, don't expect this to work so well across a full release upgrade.


## Is this serious? Who is this for?

Consider it a proof of concept and an experiment in reducing the size of Nix
downloads (and maybe one day cache + local storage too).

This project should be most interesting to Nix users with a slow or metered download connection.
If you're on a fast connection, you won't see much or any benefit.
Like all compression, we're trading CPU for bandwidth,
but there are some significant downsides to this method (see below).


## Does this really work?

I've been getting a 5–15× overall reduction in download size for routine large NixOS updates.

For example, here's the analysis of a recent NixOS update that included a merge of staging,
invalidating a chunk of my system closure:

```
======== log/system-upgrade-20230904-9075cba53e8-da5adce0ffa.jsonl
time range from 2023-09-04T20:59:26Z to 2023-09-04T21:00:54Z = 88.0s
324 total requested  287 diffed  0 eq  14 not found  20 too small  0 too big  0 no base
uncmp nar 3837463944  cmp nar 971969260  delta size 63540230  ratio 15.30
actual dl 71381554  actual ratio 13.62  cmp nar dl at 40Mbps 194.4s
compress t:21.7 u:9.5 s:8.2  expand t:90.7 u:1.7 s:9.5
```

Downloading compressed nar files would have take 927MiB, about 194 seconds on my 40Mbps connection.
What was actually downloaded took only 68MiB, more than 13× smaller.
That's 61MiB of binary deltas and 7MiB where it fell back to compressed nars for various reasons.
The upgrade overall took only 88 seconds, 55% less time.

(It doesn't take 13× less time because computing and applying the deltas takes some time.)


## What's the catch?

Of course there's a catch: someone has to compute the binary deltas,
and they have to do it close to the source (upstream cache) otherwise the savings are negated.
Computing and serving binary deltas is not _that_ expensive,
but it is not quite as cheap as serving static files.

(Caching or pre-computation is possible but challenging,
since the delta depends on both the base and target,
so you have an essentially quadratic number of items to store.
You could try to form chains of deltas to reduce the number of items,
but this reduces the benefits and keeping track of the intermediate points is hard.)

nix-sandwich does the simplest possible thing: it computes deltas on-demand.

It can run anywhere you like, but it's particularly suited for FaaS platforms
like AWS Lambda, since the workload (for any one user) is quite bursty.

### How expensive is it?

My personal usage, updating three machines running NixOS every week or so,
with a ~10GB system closure, fit well well under the Lambda free tier (1-2% of it),
so it didn't cost anything.
Without the free tier, it would have cost about $0.10 USD/month.

Even at cents per month user, it's unlikely that anyone would be
willing to offer this as a free service like the current public binary cache.
It's possible that paid binary cache services like Cachix could offer this, though.


## How do I use it?

You'll have to run your own instance of the binary delta service.
I wrote some scripts to set it up on AWS Lambda.

This assumes some basic familiarity with AWS and Terraform.
Note that I am not an AWS or Terraform expert and this could be greatly improved!
Also note that there's currently no authentication besides the obscurity of the Lambda URL!

1. Get some AWS credentials in your environment to set up the Lambda function.
   You might want to create a new IAM role with administrator access and a new
   profile for it.
2. Run `terraform -chdir=tf apply -target aws_ecr_repository.repo -var differ_image_tag=dummy`.
   This will create an ECR repo to hold the image.
3. Run `scripts/deploy-lambda.sh`. This will build the image and deploy the Lambda function.
   It will print the Lambda function URL.
4. Import or copy `module.nix` in your NixOS configuration, and set your substituter URL:
   `systemd.services.nix-sandwich.environment.nix_sandwich_differ = "https://my-lambda-function-url.lambda-url.us-east-1.on.aws/";`
   Note that it just passes on signatures from the upstream binary cache,
   so no new keys are required.
5. Add `--option extra-substituters http://localhost:7419` to your `nixos-rebuild` command line.
   (I use a wrapper script that does this automatically if something is listening on that port.)


## How does it work in detail?

There's a part that runs on your machine,
that acts as a Nix binary cache, and also a client to the delta server.
The delta server part runs in Lambda
(or as a standalone server, or in the same process for testing)
and accepts requests from the substituter to compute binary deltas between two
nars fetched from an upstream cache.

When Nix asks the local substituter if it has a particular narinfo,
it asks the upstream cache first (`cache.nixos.org` by default).
If it finds it there, it tries to figure out a suitable "base" for a binary
delta using some heuristics and ugly hacks.
If it finds a base, it returns the upstream narinfo (slightly modified)
and records a few pieces of information in memory.
If anything fails, it just says it doesn't have the narinfo and Nix tries the
next cache or builds it locally.

When Nix then asks for the nar itself,
it asks the delta server to compute a binary delta between the base and the real requested nar.
Then it recreates a nar from the base (using `nix-store --dump`),
applies the binary delta, and returns the result to Nix.

The delta server implements a simple protocol over http:
A request has a base store path, a requested store path, and a few other useful pieces of data.
It downloads nar files for both the base and requested store path and computes a binary delta.
All (or at least all recent) nar files in the main binary cache are compressed with xz,
so it has to un-xz them first.

### What does it use to compute binary deltas?

Two programs are currently supported: zstd (using `--patch-from`) and xdelta3.
xdelta3 (at levels 6+) generally produces smaller deltas,
especially [when the inputs are nearly identical][bug1],
but zstd (at low levels) is _much_ faster and pretty still good (and better on some files).
The default is set to zstd at level 3.
Higher levels of zstd produce slightly smaller deltas but take so much longer that total update time is worse.

[bug1]: https://github.com/facebook/zstd/issues/2576#issuecomment-818927743

I didn't evalulate bsdiff, jojodiff, hdiff, courgette, etc. since they weren't
packaged in nixpkgs, and also based on what I've read, all of those tools
optimize for producing smaller deltas at the cost of speed. Since we're
computing deltas on-demand, the speed of zstd is more important.

### How does it pick base packages?

1. By looking at the name and number of dashes (e.g. base `git-2.38.5` on
   `git-2.38.4` and not `git-2.38.4-doc`)
2. By looking at the system a package is built for
   (e.g. don't base 64-bit `python3-3.10.12` on a 32-bit `python3-3.10.11`)

Note that getting the system a package is built for is not easy!
nix-sandwich uses an ugly hack but if anyone knows a better way,
please share.

I had some more clever ideas in mind but those two do a good enough job.

### Embedded compressed files

Did you ever look inside a NixOS Linux kernel package?
It's mostly `.ko` files compressed with xz.
Those are understandably not going to diff well.
But the kernel is pretty big and updated frequently. What can we do?

We can expand all the modules, compute a binary delta on that, and then compress them again.

If you think about how this has to happen in the context of binary substitution,
you might realize this is really fragile!
What if the re-compression doesn't produce the same output as the original compression?
It _is_ fragile, but it works (for now):
as long as we're careful to use the same options as the Linux build, xz produces bit-identical output.
Some of the required options can be determined from the xz file directly,
and the rest are (thankfully) the defaults.

This partly relies on the fact that xz is stable software and isn't changing
output from version to version.
If the kernel switches to using zstd, or xz gets some upgrades such that the
recompressed files don't match,
we could switch to signing the nar instead of relying on the original binary cache's signature.
(Though this means we have to compute the diff at narinfo request time.)

We can use the same trick to handle `-man` packages that have gzip-compressed man pages.
(We could also use it on any package that has compressed gzip or xz files,
but there's not enough gain for the risk.)


## Future work

- Binary caches could _store_ binary deltas for nars instead of storing whole (compressed) nars.
  This could reduce storage requirements significantly, but you'd need to come
  up with a scheme for storing deltas and occasional checkpoints so that any nar
  (or nar delta) could be easily constructed. In this case, it might make sense
  to use vcdiff (produced by xdelta3 and others) as the delta format, since
  vcdiffs can be composed quickly and without access to the base.

- _Local_ Nix stores could store binary deltas instead of multiple copies of very
  similar files. This would require filesystem support (possibly FUSE) and would
  probably only make sense for less-frequently used parts of the store.


## Related work

- Arch Linux tried using delta compression for distributing updates,
  and abandoned the experiment. I can't find much information about this,
  but my understanding is they precomputed them and served them as static files.

- [nix-casync][casync] and new Nix store protocols like [tvix-store][tvix]:
  These projects use content-defined chunking.
  There are several major advantages of this over nix-sandwich,
  and a few disadvantages:

  - Pro: Easy to pre-compute and serve from static file hosting (this is critical).
  - Pro: Chunks can be shared between different store paths.
  - Con: Transfers more data overall, less efficient "compression".
  - Con: Need to keep a separate chunk store locally.

  Overall, the ability to serve from static file hosting overrides everything else,
  so if Nix is going to replace the substitution protocol eventually,
  this seems to be the direction it should go in.

- [Attic binary cache][attic], also using content-defined chunking.

- Some current (Oct/Nov 2023) [deduplication efforts around the main binary cache][dedup].
  Also appears to be using content-defined chunking.

- [SDCH][sdch], the inspiration for the name.

## License

Distributed under the Apache 2.0 License.

[casync]: https://github.com/flokli/nix-casync
[tvix]: https://cs.tvl.fyi/depot/-/tree/tvix
[attic]: https://github.com/zhaofengli/attic
[dedup]: https://discourse.nixos.org/t/2023-10-24-re-long-term-s3-cache-solutions-meeting-minutes-1/34580
[sdch]: https://en.wikipedia.org/wiki/SDCH

