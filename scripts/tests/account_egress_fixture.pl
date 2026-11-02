#!/usr/bin/perl
use strict;
use warnings;
use IO::Socket::INET;
use IO::Select;
use JSON::PP qw(decode_json);
use Time::HiRes qw(sleep time);

# Only synthetic test credentials and loopback sockets are used by this file.
my $key = 'synthetic-account-control-key-not-production-0123456789';
sub send_all {
    my ($socket, $data) = @_;
    while (length $data) {
        my $sent = syswrite($socket, $data);
        die "write failed" unless $sent;
        substr($data, 0, $sent, '');
    }
}
sub header {
    my ($socket) = @_;
    my $data = '';
    while ($data !~ /\r\n\r\n\z/) {
        die "header EOF" unless sysread($socket, my $byte, 1);
        $data .= $byte;
        die "header limit" if length($data) > 16384;
    }
    return $data;
}
sub exact {
    my ($socket, $count) = @_;
    my $data = '';
    while (length($data) < $count) {
        die "data EOF" unless sysread($socket, my $part, $count - length($data));
        $data .= $part;
    }
    return $data;
}
sub file {
    my ($path) = @_;
    open my $input, '<', $path or die "fixture file unavailable";
    local $/;
    return <$input>;
}
sub event {
    my ($name, $message) = @_;
    open my $out, '>>', "/run/account-$name.log" or die "fixture log unavailable";
    print $out "$message\n";
    close $out;
}

my $operation = shift @ARGV // '';
$SIG{PIPE} = 'IGNORE';
if ($operation eq 'serve') {
    my ($label, $address) = @ARGV;
    my $server = IO::Socket::INET->new(LocalAddr=>$address,LocalPort=>443,Listen=>32,ReuseAddr=>1,Proto=>'tcp') or die "listen failed";
    $SIG{CHLD} = 'IGNORE';
    while (my $client=$server->accept()) {
        my $pid=fork(); die "fork failed" unless defined $pid;
        if ($pid) {close $client; next;}
        close $server;
        event($label, 'open');
        eval { while (1) {send_all($client,$label.exact($client,4));} };
        event($label, 'close');
        close $client; exit 0;
    }
} elsif ($operation eq 'control') {
    alarm 20;
    my ($path,$method,$payload_path)=@ARGV;
    my $payload=$payload_path ? file($payload_path) : '';
    my $socket=IO::Socket::INET->new(PeerAddr=>'172.28.70.3',PeerPort=>17892,LocalAddr=>'172.28.70.2',Proto=>'tcp',Timeout=>10) or die "control connect failed";
    send_all($socket,"$method $path HTTP/1.1\r\nHost: account-egress\r\nAuthorization: Bearer $key\r\nContent-Type: application/json\r\nContent-Length: ".length($payload)."\r\nConnection: close\r\n\r\n".$payload);
    my $response=header($socket);
    die "control response invalid" unless $response =~ m{\AHTTP/1\.[01] (\d+)};
    my $status=$1;my $body='';while(sysread($socket,my $part,65536)){$body.=$part;}
    print "$status\n$body";
} elsif ($operation eq 'probe') {
    alarm 25;
    my ($snapshot_path,$route_index,$label,$mode)=@ARGV;
    $mode //= 'once';
    my $snapshot=decode_json(file($snapshot_path));
    my $route=$snapshot->{routes}[$route_index];
    require MIME::Base64;
    my $auth=MIME::Base64::encode_base64($route->{id}.':'.$route->{token},'');
    if ($mode eq 'bad-token') {$auth=MIME::Base64::encode_base64($route->{id}.':invalid','');}
    my $socket=IO::Socket::INET->new(PeerAddr=>'172.28.30.4',PeerPort=>3129,LocalAddr=>'127.0.0.2',Proto=>'tcp',Timeout=>8) or die "proxy connect failed";
    my $credentials=$mode eq 'missing-token' ? '' : "Proxy-Authorization: Basic $auth\r\n";
    my $target=$mode eq 'bad-host' ? 'evil.invalid' : 'chatgpt.com';
    send_all($socket,"CONNECT $target:443 HTTP/1.1\r\nHost: $target:443\r\n$credentials\r\n");
    my $response=header($socket);
    die "CONNECT response invalid" unless $response =~ m{\AHTTP/1\.[01] (\d+)};
    my $status=$1; print "$status\n";
    exit 0 unless $status==200;
    if ($mode eq 'reject-tunnel') {
        my $ok=eval {send_all($socket,'ping');exact($socket,5);1};
        die "failed node unexpectedly reached an origin" if $ok;
        print "BLOCKED\n";exit 0;
    }
    my $count=$mode eq 'stream' ? 80 : 2;
    for (1..$count) {
        send_all($socket,'ping');
        die "wrong SS exit" unless exact($socket,5) eq $label.'ping';
        if ($mode eq 'stream' && $_==1) {open my $ready,'>', '/run/account-stream-ready' or die "ready file failed";close $ready;}
        sleep 0.05 if $mode eq 'stream';
    }
    print "EXIT_$label\n";
} else {die "unsupported fixture operation";}
