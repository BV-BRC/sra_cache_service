package SRACacheClient;

use POSIX;
use strict;
use Data::Dumper;
use URI;

use Time::HiRes qw(gettimeofday);

use P3AuthToken;


=head1 NAME

SRACacheClient

=head1 DESCRIPTION


SRACache: precache and serve NCBI SRA run data, and answer SRA run
metadata queries, for BV-BRC tooling built on top of the SRA toolkit
(prefetch / fasterq-dump / fastq-dump).
This is a direct transcription of section 6 ("API sketch") of
PLAN-sra-precache-service.md (dev_container modules tree) into TypeSpec.
Do not add fields beyond what that design doc specifies without updating
it first -- every field here traces to a decision recorded there.
Control plane only. The data plane (plain HTTP, streaming cached .sra
bytes via the signed URL get_sra_data mints) is a separate, non-JSONRPC
listener -- see the implementation plan's Phase 7.


=cut


sub new
{
    my($class, $url, @args) = @_;
    
    if (!defined($url))
    {
	$url = 'https://p3.theseed.org/services/SRACache';
    }

    my $self = {
	client => SRACacheClient::RpcClient->new,
	url => $url,
	headers => [],
    };

    chomp($self->{hostname} = `hostname`);
    $self->{hostname} ||= 'unknown-host';

    #
    # Set up for propagating KBRPC_TAG and KBRPC_METADATA environment variables through
    # to invoked services. If these values are not set, we create a new tag
    # and a metadata field with basic information about the invoking script.
    #
    if ($ENV{KBRPC_TAG})
    {
	$self->{kbrpc_tag} = $ENV{KBRPC_TAG};
    }
    else
    {
	my ($t, $us) = gettimeofday;
	$us = sprintf("%06d", $us);
	my $ts = strftime("%Y-%m-%dT%H:%M:%S.${us}Z", gmtime $t);
	$self->{kbrpc_tag} = "C:$0:$self->{hostname}:$$:$ts";
    }
    push(@{$self->{headers}}, 'Kbrpc-Tag', $self->{kbrpc_tag});

    if ($ENV{KBRPC_METADATA})
    {
	$self->{kbrpc_metadata} = $ENV{KBRPC_METADATA};
	push(@{$self->{headers}}, 'Kbrpc-Metadata', $self->{kbrpc_metadata});
    }

    if ($ENV{KBRPC_ERROR_DEST})
    {
	$self->{kbrpc_error_dest} = $ENV{KBRPC_ERROR_DEST};
	push(@{$self->{headers}}, 'Kbrpc-Errordest', $self->{kbrpc_error_dest});
    }

    #
    # This module requires authentication.
    #
    # We create an auth token, passing through the arguments that we were (hopefully) given.

    {
	my $token = P3AuthToken->new(@args);
	
	if (my $token_str = $token->token())
	{
	    $self->{token} = $token_str;
	    $self->{client}->{token} = $token_str;
	}
    }

    my $ua = $self->{client}->ua;	 
    my $timeout = $ENV{CDMI_TIMEOUT} || (30 * 60);	 
    $ua->timeout($timeout);
    $ua->agent("SRACacheClient UserAgent");
    bless $self, $class;
    return $self;
}




=head2 start_precache

  $status = $obj->start_precache($ids, $options)

=over 4

=item Parameter and return types

=begin html

<pre>
$ids is a reference to a list where each element is a sra_accession
$options is a precache_options
$status is a reference to a list where each element is a precache_status
sra_accession is a string
precache_options is a reference to a hash where the following keys are defined:
	priority has a value which is an int
precache_status is a reference to a hash where the following keys are defined:
	accession has a value which is a sra_accession
	state has a value which is a precache_state
	message has a value which is a string
	size_bytes has a value which is an int
	queued_time has a value which is a timestamp
	cached_time has a value which is a timestamp
precache_state is a string
timestamp is a string

</pre>

=end html

=begin text

$ids is a reference to a list where each element is a sra_accession
$options is a precache_options
$status is a reference to a list where each element is a precache_status
sra_accession is a string
precache_options is a reference to a hash where the following keys are defined:
	priority has a value which is an int
precache_status is a reference to a hash where the following keys are defined:
	accession has a value which is a sra_accession
	state has a value which is a precache_state
	message has a value which is a string
	size_bytes has a value which is an int
	queued_time has a value which is a timestamp
	cached_time has a value which is a timestamp
precache_state is a string
timestamp is a string


=end text

=item Description

Queue a list of run-level accessions (SRR/DRR/ERR) for precaching.
       SRX/SRP accessions are rejected with state = invalid_id -- expand them
       via get_metadata first (see the design doc, section 6).

=back

=cut

sub start_precache
{
    my($self, @args) = @_;

# Authentication: required

    if ((my $n = @args) != 2)
    {
        die "Invalid argument count for function start_precache (received $n, expecting 2)";
    }
    {
	my($ids, $options) = @args;

	my @_bad_arguments;
        (ref($ids) eq 'ARRAY') or push(@_bad_arguments, "Invalid type for argument 1 \"ids\" (value was \"$ids\")");
        (ref($options) eq 'HASH') or push(@_bad_arguments, "Invalid type for argument 2 \"options\" (value was \"$options\")");
        if (@_bad_arguments) {
	    my $msg = "Invalid arguments passed to start_precache:\n" . join("", map { "\t$_\n" } @_bad_arguments);
	    die $msg;
	}
    }

    my $result = $self->{client}->call($self->{url}, $self->{headers}, {
	method => "SRACache.start_precache",
	params => \@args,
    });
    if ($result) {
	if ($result->{error}) {
	    my $msg = $result->{error}->{error} || $result->{error}->{message};
	    $msg =  $self->{client}->json->encode($msg) if ref($msg);
	    die "Error $result->{error}->{code} invoking start_precache:\n$msg\n";
	} else {
	    return wantarray ? @{$result->{result}} : $result->{result}->[0];
	}
    } else {
	die "Error invoking method start_precache: " .  $self->{client}->status_line;
    }
}



=head2 precache_status

  $status = $obj->precache_status($ids)

=over 4

=item Parameter and return types

=begin html

<pre>
$ids is a reference to a list where each element is a sra_accession
$status is a reference to a list where each element is a precache_status
sra_accession is a string
precache_status is a reference to a hash where the following keys are defined:
	accession has a value which is a sra_accession
	state has a value which is a precache_state
	message has a value which is a string
	size_bytes has a value which is an int
	queued_time has a value which is a timestamp
	cached_time has a value which is a timestamp
precache_state is a string
timestamp is a string

</pre>

=end html

=begin text

$ids is a reference to a list where each element is a sra_accession
$status is a reference to a list where each element is a precache_status
sra_accession is a string
precache_status is a reference to a hash where the following keys are defined:
	accession has a value which is a sra_accession
	state has a value which is a precache_state
	message has a value which is a string
	size_bytes has a value which is an int
	queued_time has a value which is a timestamp
	cached_time has a value which is a timestamp
precache_state is a string
timestamp is a string


=end text

=item Description

Poll the current precache state of a list of run-level accessions.

=back

=cut

sub precache_status
{
    my($self, @args) = @_;

# Authentication: optional

    if ((my $n = @args) != 1)
    {
        die "Invalid argument count for function precache_status (received $n, expecting 1)";
    }
    {
	my($ids) = @args;

	my @_bad_arguments;
        (ref($ids) eq 'ARRAY') or push(@_bad_arguments, "Invalid type for argument 1 \"ids\" (value was \"$ids\")");
        if (@_bad_arguments) {
	    my $msg = "Invalid arguments passed to precache_status:\n" . join("", map { "\t$_\n" } @_bad_arguments);
	    die $msg;
	}
    }

    my $result = $self->{client}->call($self->{url}, $self->{headers}, {
	method => "SRACache.precache_status",
	params => \@args,
    });
    if ($result) {
	if ($result->{error}) {
	    my $msg = $result->{error}->{error} || $result->{error}->{message};
	    $msg =  $self->{client}->json->encode($msg) if ref($msg);
	    die "Error $result->{error}->{code} invoking precache_status:\n$msg\n";
	} else {
	    return wantarray ? @{$result->{result}} : $result->{result}->[0];
	}
    } else {
	die "Error invoking method precache_status: " .  $self->{client}->status_line;
    }
}



=head2 get_metadata

  $metadata = $obj->get_metadata($ids)

=over 4

=item Parameter and return types

=begin html

<pre>
$ids is a reference to a list where each element is a sra_accession
$metadata is a reference to a list where each element is a sra_metadata
sra_accession is a string
sra_metadata is a reference to a hash where the following keys are defined:
	accession has a value which is a sra_accession
	platform has a value which is a string
	library_layout has a value which is a string
	n_reads has a value which is an int
	size_bytes has a value which is an int
	extra has a value which is a reference to a hash where the key is a string and the value is a string

</pre>

=end html

=begin text

$ids is a reference to a list where each element is a sra_accession
$metadata is a reference to a list where each element is a sra_metadata
sra_accession is a string
sra_metadata is a reference to a hash where the following keys are defined:
	accession has a value which is a sra_accession
	platform has a value which is a string
	library_layout has a value which is a string
	n_reads has a value which is an int
	size_bytes has a value which is an int
	extra has a value which is a reference to a hash where the key is a string and the value is a string


=end text

=item Description

Accepts any accession type (run, experiment, or study) and always
       returns run-level records -- an experiment/study id expands to one
       record per constituent run, matching
       sra_tools.parse_accession_metadata's existing behavior.

=back

=cut

sub get_metadata
{
    my($self, @args) = @_;

# Authentication: optional

    if ((my $n = @args) != 1)
    {
        die "Invalid argument count for function get_metadata (received $n, expecting 1)";
    }
    {
	my($ids) = @args;

	my @_bad_arguments;
        (ref($ids) eq 'ARRAY') or push(@_bad_arguments, "Invalid type for argument 1 \"ids\" (value was \"$ids\")");
        if (@_bad_arguments) {
	    my $msg = "Invalid arguments passed to get_metadata:\n" . join("", map { "\t$_\n" } @_bad_arguments);
	    die $msg;
	}
    }

    my $result = $self->{client}->call($self->{url}, $self->{headers}, {
	method => "SRACache.get_metadata",
	params => \@args,
    });
    if ($result) {
	if ($result->{error}) {
	    my $msg = $result->{error}->{error} || $result->{error}->{message};
	    $msg =  $self->{client}->json->encode($msg) if ref($msg);
	    die "Error $result->{error}->{code} invoking get_metadata:\n$msg\n";
	} else {
	    return wantarray ? @{$result->{result}} : $result->{result}->[0];
	}
    } else {
	die "Error invoking method get_metadata: " .  $self->{client}->status_line;
    }
}



=head2 get_sra_data

  $locations = $obj->get_sra_data($ids)

=over 4

=item Parameter and return types

=begin html

<pre>
$ids is a reference to a list where each element is a sra_accession
$locations is a reference to a list where each element is a sra_data_location
sra_accession is a string
sra_data_location is a reference to a hash where the following keys are defined:
	accession has a value which is a sra_accession
	state has a value which is a precache_state
	url has a value which is a string
	expires has a value which is a timestamp
precache_state is a string
timestamp is a string

</pre>

=end html

=begin text

$ids is a reference to a list where each element is a sra_accession
$locations is a reference to a list where each element is a sra_data_location
sra_accession is a string
sra_data_location is a reference to a hash where the following keys are defined:
	accession has a value which is a sra_accession
	state has a value which is a precache_state
	url has a value which is a string
	expires has a value which is a timestamp
precache_state is a string
timestamp is a string


=end text

=item Description

Get (and, if necessary, trigger caching of) run-level accession data.
       If an accession isn't cached yet, this call enqueues it as a side
       effect and returns its current state with an empty url; poll again
       once state == cached to get a usable url.

=back

=cut

sub get_sra_data
{
    my($self, @args) = @_;

# Authentication: optional

    if ((my $n = @args) != 1)
    {
        die "Invalid argument count for function get_sra_data (received $n, expecting 1)";
    }
    {
	my($ids) = @args;

	my @_bad_arguments;
        (ref($ids) eq 'ARRAY') or push(@_bad_arguments, "Invalid type for argument 1 \"ids\" (value was \"$ids\")");
        if (@_bad_arguments) {
	    my $msg = "Invalid arguments passed to get_sra_data:\n" . join("", map { "\t$_\n" } @_bad_arguments);
	    die $msg;
	}
    }

    my $result = $self->{client}->call($self->{url}, $self->{headers}, {
	method => "SRACache.get_sra_data",
	params => \@args,
    });
    if ($result) {
	if ($result->{error}) {
	    my $msg = $result->{error}->{error} || $result->{error}->{message};
	    $msg =  $self->{client}->json->encode($msg) if ref($msg);
	    die "Error $result->{error}->{code} invoking get_sra_data:\n$msg\n";
	} else {
	    return wantarray ? @{$result->{result}} : $result->{result}->[0];
	}
    } else {
	die "Error invoking method get_sra_data: " .  $self->{client}->status_line;
    }
}





=head1 TYPES



=head2 sra_accession

=over 4



=item Description

An SRA accession: run (SRR/DRR/ERR), experiment (SRX/DRX/ERX), or
       study/project (SRP/DRP/ERP).


=item Definition

=begin html

<pre>
a string
</pre>

=end html

=begin text

a string

=end text

=back



=head2 timestamp

=over 4



=item Definition

=begin html

<pre>
a string
</pre>

=end html

=begin text

a string

=end text

=back



=head2 precache_state

=over 4



=item Description

queued      - accepted, not yet started
       in_progress - prefetch running
       cached      - .sra present and verified in local cache
       invalid_id  - fails accession syntax check (fast, no NCBI round trip),
                     or (for start_precache/get_sra_data) is not a run-level
                     accession
       not_found   - well-formed id, NCBI has no such accession
       error       - prefetch failed; message set; caller may retry


=item Definition

=begin html

<pre>
a string
</pre>

=end html

=begin text

a string

=end text

=back



=head2 precache_status

=over 4



=item Description

Per-accession precache status.

      sra_accession accession - the accession this status is for
      precache_state state - current state, see precache_state
      string message - human-readable detail; set for invalid_id/not_found/error
      int size_bytes - size of the cached .sra file; 0 until cached
      timestamp queued_time - when the accession was queued
      timestamp cached_time - when the accession finished caching; unset until cached


=item Definition

=begin html

<pre>
a reference to a hash where the following keys are defined:
accession has a value which is a sra_accession
state has a value which is a precache_state
message has a value which is a string
size_bytes has a value which is an int
queued_time has a value which is a timestamp
cached_time has a value which is a timestamp

</pre>

=end html

=begin text

a reference to a hash where the following keys are defined:
accession has a value which is a sra_accession
state has a value which is a precache_state
message has a value which is a string
size_bytes has a value which is an int
queued_time has a value which is a timestamp
cached_time has a value which is a timestamp


=end text

=back



=head2 precache_options

=over 4



=item Description

Options controlling a precache request.

      int priority - relative scheduling priority; higher runs sooner


=item Definition

=begin html

<pre>
a reference to a hash where the following keys are defined:
priority has a value which is an int

</pre>

=end html

=begin text

a reference to a hash where the following keys are defined:
priority has a value which is an int


=end text

=back



=head2 sra_metadata

=over 4



=item Description

Run-level SRA metadata, reconciled against real p3-sra consumers
       (design doc section 7.2) rather than exposing every field
       sra_tools.parse_accession_metadata computes. Only the fields with a
       confirmed reader are typed; everything else lands in extra.

       sra_accession accession - always a run-level (SRR/DRR/ERR) accession,
           even when the request named an experiment or study
       string platform - NCBI's raw PLATFORM tag (ILLUMINA, PACBIO_SMRT,
           OXFORD_NANOPORE, ION_TORRENT, ...), not canonicalized
       string library_layout - "PAIRED" or "SINGLE"
       int n_reads - read count, using the Statistics-children-with-count>0
           rule (the naive @nreads attribute lies for some runs)
       int size_bytes - run size in bytes
       mapping<string,string> extra - every other field
           parse_accession_metadata computes today (exp_id, study_id,
           instrument_model, library_strategy/selection, sample_*, spots,
           bases, estimated_size, read_length, ...), carried through
           uninterpreted since nothing in this codebase reads them yet


=item Definition

=begin html

<pre>
a reference to a hash where the following keys are defined:
accession has a value which is a sra_accession
platform has a value which is a string
library_layout has a value which is a string
n_reads has a value which is an int
size_bytes has a value which is an int
extra has a value which is a reference to a hash where the key is a string and the value is a string

</pre>

=end html

=begin text

a reference to a hash where the following keys are defined:
accession has a value which is a sra_accession
platform has a value which is a string
library_layout has a value which is a string
n_reads has a value which is an int
size_bytes has a value which is an int
extra has a value which is a reference to a hash where the key is a string and the value is a string


=end text

=back



=head2 sra_data_location

=over 4



=item Description

Where to fetch a cached run's data once it's ready.

      sra_accession accession - the run-level accession this location is for
      precache_state state - reflects whether a precache was already
          complete or was just triggered as a side effect of this call
      string url - a short-lived signed data-plane URL; only valid once
          state == cached
      timestamp expires - when url stops being valid; also the eviction
          lease pinning this accession in the cache until then


=item Definition

=begin html

<pre>
a reference to a hash where the following keys are defined:
accession has a value which is a sra_accession
state has a value which is a precache_state
url has a value which is a string
expires has a value which is a timestamp

</pre>

=end html

=begin text

a reference to a hash where the following keys are defined:
accession has a value which is a sra_accession
state has a value which is a precache_state
url has a value which is a string
expires has a value which is a timestamp


=end text

=back



=cut

package SRACacheClient::RpcClient;
use POSIX;
use strict;
use LWP::UserAgent;
use JSON::XS;

BEGIN {
    for my $method (qw/uri ua json content_type version id allow_call status_line/) {
	eval qq|
	    sub $method {
		\$_[0]->{$method} = \$_[1] if defined \$_[1];
		\$_[0]->{$method};
	    }
	    |;
	}
    }

sub new
{
    my($class) = @_;

    my $ua = LWP::UserAgent->new();
    my $json = JSON::XS->new->allow_nonref->utf8;
    
    my $self = {
	ua => $ua,
	json => $json,
    };
    return bless $self, $class;
}

sub call {
    my ($self, $uri, $headers, $obj) = @_;
    my $result;


    {
	if ($uri =~ /\?/) {
	    $result = $self->_get($uri);
	}
	else {
	    Carp::croak "not hashref." unless (ref $obj eq 'HASH');
	    $result = $self->_post($uri, $headers, $obj);
	}

    }

    my $service = $obj->{method} =~ /^system\./ if ( $obj );

    $self->status_line($result->status_line);

    if ($result->is_success || $result->content_type eq 'application/json') {

	my $txt = $result->content;

        return unless($txt); # notification?

	my $obj = eval { $self->json->decode($txt); };

	if (!$obj)
	{
	    die "Error parsing result: $@";
	}

	return $obj;
    }
    else {
        return;
    }
}

sub _get {
    my ($self, $uri) = @_;
    $self->ua->get(
		   $uri,
		   Accept         => 'application/json',
		  );
}

sub _post {
    my ($self, $uri, $headers, $obj) = @_;
    my $json = $self->json;

    $obj->{version} ||= $self->{version} || '1.1';

    if ($obj->{version} eq '1.0') {
        delete $obj->{version};
        if (exists $obj->{id}) {
            $self->id($obj->{id}) if ($obj->{id}); # if undef, it is notification.
        }
        else {
            $obj->{id} = $self->id || ($self->id('JSON::RPC::Legacy::Client'));
        }
    }
    else {
        # $obj->{id} = $self->id if (defined $self->id);
	# Assign a random number to the id if one hasn't been set
	$obj->{id} = (defined $self->id) ? $self->id : substr(rand(),2);
    }

    my $content = $json->encode($obj);

    $self->ua->post(
        $uri,
        Content_Type   => $self->{content_type},
        Content        => $content,
        Accept         => 'application/json',
	@$headers,
	($self->{token} ? (Authorization => $self->{token}) : ()),
    );
}



1;
