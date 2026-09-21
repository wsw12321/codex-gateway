#!/usr/bin/perl
use strict;
use warnings;
use IO::Socket::INET;
use IO::Select;
use POSIX qw(setpgid);
use Time::HiRes qw(time sleep);

# All sockets stay inside a Docker --network none loopback namespace. The
# pinned Squid image already includes Perl, so no additional image is needed.
sub event {
    my ($name, $message) = @_;
    open my $log, '>>', "/run/fixture-$name.log" or die $!;
    print $log "$message\n";
    close $log;
}

sub header {
    my ($socket) = @_;
    my $text = '';
    while ($text !~ /\r\n\r\n\z/) {
        die "unexpected EOF reading headers" unless sysread($socket, my $byte, 1);
        $text .= $byte;
        die "oversized headers" if length($text) > 16384;
    }
    return $text;
}

sub send_all {
    my ($socket, $data) = @_;
    while (length $data) {
        my $written = syswrite($socket, $data);
        die "socket write: $!" unless $written;
        substr($data, 0, $written, '');
    }
}

sub receive_exact {
    my ($socket, $length) = @_;
    my $data = '';
    while (length($data) < $length) {
        die "unexpected EOF" unless sysread($socket, my $part, $length - length($data));
        $data .= $part;
    }
    return $data;
}

my $mode = shift @ARGV // '';
if ($mode eq 'serve') {
    my $name = shift @ARGV;
    setpgid(0, 0) == 0 or die "setpgid: $!";
    my $listen = IO::Socket::INET->new(
        LocalAddr => $name eq 'parent' ? '127.0.0.5' : '127.0.0.6',
        LocalPort => $name eq 'parent' ? 3129 : 443,
        Listen => 32, ReuseAddr => 1, Proto => 'tcp',
    ) or die "listen: $!";
    open my $pid, '>', "/run/fixture-$name.pid" or die $!;
    print $pid "$$\n";
    close $pid;
    $SIG{CHLD} = 'IGNORE';
    while (my $client = $listen->accept()) {
        my $child = fork();
        die "fork: $!" unless defined $child;
        if ($child) { close $client; next; }
        close $listen;
        $SIG{PIPE} = 'IGNORE';
        event($name, "open $$");
        eval {
            if ($name eq 'target') {
                while (sysread($client, my $data, 65536)) { send_all($client, $data); }
            } else {
                my $request = header($client);
                die "unexpected parent request: $request"
                    unless $request =~ m{\ACONNECT (auth\.openai\.com|chatgpt\.com):443 HTTP/1\.[01]\r\n};
                event($name, "CONNECT $1:443");
                my $upstream = IO::Socket::INET->new(
                    PeerAddr => '127.0.0.6', PeerPort => 443, Proto => 'tcp',
                ) or die "parent target: $!";
                send_all($client, "HTTP/1.1 200 Connection established\r\n\r\n");
                my $select = IO::Select->new($client, $upstream);
                FORWARD: while (my @ready = $select->can_read(10)) {
                    for my $source (@ready) {
                        last FORWARD unless sysread($source, my $data, 65536);
                        send_all(fileno($source) == fileno($client) ? $upstream : $client, $data);
                    }
                }
                close $upstream;
            }
        };
        event($name, "close $$");
        close $client;
        exit 0;
    }
} elsif ($mode eq 'stop') {
    my $name = shift @ARGV;
    open my $file, '<', "/run/fixture-$name.pid" or die $!;
    my $pid = <$file>;
    close $file;
    kill 'TERM', -int($pid) or die "kill: $!";
    unlink "/run/fixture-$name.pid";
} elsif ($mode eq 'probe') {
    my ($proxy, $source, $method, $host, $port, $operation) = @ARGV;
    $operation //= 'echo';
    $SIG{PIPE} = 'IGNORE';
    alarm 20;
    my $socket = IO::Socket::INET->new(
        PeerAddr => $proxy, PeerPort => $method eq 'DIRECT' ? 443 : 3128,
        LocalAddr => $source, Proto => 'tcp', Timeout => 8,
    ) or die "connect: $!";
    if ($method ne 'DIRECT') {
        my $destination = $method eq 'CONNECT' ? "$host:$port" : "http://$host/";
        send_all($socket, "$method $destination HTTP/1.1\r\nHost: $host:$port\r\n\r\n");
        my $response = header($socket);
        die "malformed response: $response" unless $response =~ m{\AHTTP/1\.[01] (\d+)};
        print "$1\n";
        exit 0 unless $1 == 200;
    }
    my $payload = join('', map { chr($_ % 251) } 0..65535);
    my $deadline = time + ($operation eq 'stream' ? 3 : $operation eq 'disconnect' ? 15 : 0);
    if ($operation eq 'disconnect') {
        open my $ready, '>', '/run/fixture-stream-ready' or die $!;
        close $ready;
    }
    my $completed = eval {
        do {
            send_all($socket, $payload);
            die "corrupt tunnel data" unless receive_exact($socket, length($payload)) eq $payload;
            sleep 0.03 if $operation eq 'stream' || $operation eq 'disconnect';
        } while (time < $deadline);
        1;
    };
    if ($operation eq 'disconnect') {
        die "tunnel remained open after relay stop" if $completed;
        print "DISCONNECTED\n";
    } else {
        die $@ unless $completed;
        print "ECHO_OK\n";
    }
    close $socket;
} else {
    die "unknown fixture operation: $mode";
}
