<?php
function HookRemote_assistAllHomeafterwelcometext()
    {
    # Load HTML from the ResourceSpace site, passing the base URL. The base URL allows the ResourceSpace website to feed appropriate options based
    # on whether the installation is a registered trial system, user installation, etc.
    global $baseurl;

    ?>
    <script>
    jQuery(document).ready(function() {
        if  (getCookie('remote_assist_dismissed') == 'hide') {
            jQuery('#remote_assist').hide();
            }

        if (jQuery('remote_assist').children().length==0 && getCookie('remote_assist_dismissed') != 'hide') {
            jQuery('#remote_assist').load('https://www.resourcespace.com/remote_assist_plugin.php?baseurl=<?php echo base64_encode($baseurl) ?>', function () {
                jQuery(this).prepend('<i class="icon-info"></i>');
                jQuery(this).append('<i class="icon-x" onclick="dismissRemoteAssist()"></i>');
                });
            }
        });

    function dismissRemoteAssist() {
        SetCookie ('remote_assist_dismissed', 'hide', 365)
        jQuery('#remote_assist').hide();
        }
    </script>

    <div id="remote_assist"></div>

    <?php
    }
